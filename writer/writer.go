// Copyright 2017 Jump Trading
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package writer configures and instantiates the subscribers to the
// NATS bus, in turn POST'ing data to InfluxDB.
package writer

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"
	"sync"
	"time"

	"net/http"
	_ "net/http/pprof" // for profiling a nasty memleak

	"github.com/nats-io/go-nats"

	"github.com/jumptrading/influx-spout/config"
	"github.com/jumptrading/influx-spout/filter"
	"github.com/jumptrading/influx-spout/stats"
)

// Writer stats counters
const (
	statReceived      = "received"
	statWriteRequests = "write_requests"
	statFailedWrites  = "failed_writes"
	statMaxPending    = "max_pending"
)

type Writer struct {
	c             *config.Config
	url           string
	batchMaxBytes int
	batchMaxAge   time.Duration
	nc            *nats.Conn
	rules         *filter.RuleSet
	stats         *stats.Stats
	wg            sync.WaitGroup
	stop          chan struct{}
}

// StartWriter is the heavylifter, subscribes to the subject where
// listeners publish the messages and writes it the InfluxDB endpoint.
func StartWriter(c *config.Config) (_ *Writer, err error) {
	w := &Writer{
		c:             c,
		url:           fmt.Sprintf("http://%s:%d/write?db=%s", c.InfluxDBAddress, c.InfluxDBPort, c.DBName),
		batchMaxBytes: c.BatchMaxMB * 1024 * 1024,
		batchMaxAge:   time.Duration(c.BatchMaxSecs) * time.Second,
		stats:         stats.New(statReceived, statWriteRequests, statFailedWrites, statMaxPending),
		stop:          make(chan struct{}),
	}
	defer func() {
		if err != nil {
			w.Stop()
		}
	}()

	go http.ListenAndServe(":8080", nil) // for pprof profiling

	w.rules, err = filter.RuleSetFromConfig(c)
	if err != nil {
		return nil, err
	}

	w.nc, err = nats.Connect(c.NATSAddress)
	if err != nil {
		return nil, fmt.Errorf("NATS Error: can't connect: %v", err)
	}

	// if we disconnect, we want to try reconnecting as many times as
	// we can
	w.nc.Opts.MaxReconnect = -1

	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 100

	jobs := make(chan *nats.Msg, 1024)
	w.wg.Add(w.c.Workers)
	for wk := 0; wk < w.c.Workers; wk++ {
		go w.worker(jobs)
	}

	// subscribe this writer to the NATS subject.
	maxPendingBytes := c.NATSPendingMaxMB * 1024 * 1024
	for _, subject := range c.NATSSubject {
		sub, err := w.nc.Subscribe(subject, func(msg *nats.Msg) {
			jobs <- msg
		})
		if err != nil {
			return nil, fmt.Errorf("subscription for %q failed: %v", subject, err)
		}
		if err := sub.SetPendingLimits(-1, maxPendingBytes); err != nil {
			return nil, fmt.Errorf("failed to set pending limits: %v", err)
		}

		w.wg.Add(1)
		go w.monitorSub(sub)
	}

	// Subscriptions don't always seem to be reliable without flushing
	// after subscribing.
	if err := w.nc.Flush(); err != nil {
		return nil, fmt.Errorf("NATS flush error: %v", err)
	}

	w.wg.Add(1)
	go w.startStatistician()

	log.Printf("writer subscribed to [%v] at %s with %d workers",
		c.NATSSubject, c.NATSAddress, c.Workers)
	log.Printf("POST timeout: %ds", c.WriteTimeoutSecs)
	log.Printf("maximum NATS subject size: %dMB", c.NATSPendingMaxMB)

	return w, nil
}

// Stop aborts all goroutines belonging to the Writer and closes its
// connection to NATS. It will be block until all Writer goroutines
// have stopped.
func (w *Writer) Stop() {
	close(w.stop)
	w.wg.Wait()
	if w.nc != nil {
		w.nc.Close()
	}
}

func (w *Writer) worker(jobs <-chan *nats.Msg) {
	defer w.wg.Done()

	tr := &http.Transport{
		MaxIdleConns:       10,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   time.Duration(w.c.WriteTimeoutSecs) * time.Second,
	}

	batch := newBatchBuffer()
	batchWrite := w.getBatchWriteFunc(batch)
	for {
		select {
		case j := <-jobs:
			w.stats.Inc(statReceived)
			batchWrite(j.Data)
		case <-time.After(time.Second):
			// Wake up regularly to check batch age
		case <-w.stop:
			return
		}

		if w.shouldSendBatch(batch) {
			w.stats.Inc(statWriteRequests)

			if err := w.sendBatch(batch, client); err != nil {
				w.stats.Inc(statFailedWrites)
				log.Printf("Error: %v", err)
			}

			// Reset buffer on success or error; batch will not be sent again.
			batch.Reset()
		}
	}
}

func (w *Writer) getBatchWriteFunc(batch *batchBuffer) func([]byte) {
	batchWrite := func(data []byte) {
		if err := batch.Write(data); err != nil {
			log.Printf("Error: %v", err)
		}
	}

	if w.rules.Count() == 0 {
		// No rules - just append the received data straight onto the
		// batch buffer.
		return batchWrite
	}

	return func(data []byte) {
		// Rules exist - split the received data into lines and apply
		// filters.
		for _, line := range bytes.SplitAfter(data, []byte("\n")) {
			if w.filterLine(line) {
				batchWrite(line)
			}
		}
	}
}

func (w *Writer) filterLine(line []byte) bool {
	if len(line) == 0 {
		return false
	}
	return w.rules.Lookup(line) != -1
}

func (w *Writer) shouldSendBatch(batch *batchBuffer) bool {
	return batch.Writes() >= w.c.BatchMessages ||
		batch.Size() >= w.batchMaxBytes ||
		batch.Age() >= w.batchMaxAge
}

// sendBatch sends the accumulated batch via HTTP to InfluxDB.
func (w *Writer) sendBatch(batch *batchBuffer, client *http.Client) error {
	resp, err := client.Post(w.url, "application/json; charset=UTF-8", batch.Data())
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %v\n", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode > 300 {
		errText := fmt.Sprintf("received HTTP %v from %v", resp.Status, w.url)
		if w.c.Debug {
			body, err := ioutil.ReadAll(resp.Body)
			if err == nil {
				errText += fmt.Sprintf("\nresponse body: %s\n", body)
			}
		}
		return errors.New(errText)
	}

	return nil
}

func (w *Writer) signalDrop(subject string, drop, last int) {
	// uh, this writer is overloaded and had to drop a packet
	log.Printf("Warning: dropped %d for subject %q (total dropped: %d)", drop-last, subject, drop)

	labels := w.metricsLabels()
	labels["subject"] = subject

	line := stats.CounterToPrometheus("dropped", drop, time.Now(), labels)
	w.nc.Publish(w.c.NATSSubjectMonitor, line)
	w.nc.Flush()
}

func (w *Writer) monitorSub(sub *nats.Subscription) {
	defer w.wg.Done()

	last, err := sub.Dropped()
	if err != nil {
		log.Printf("NATS Warning: Failed to get the number of dropped message from NATS: %v\n", err)
	}
	drop := last

	for {
		_, maxBytes, err := sub.MaxPending()
		if err != nil {
			log.Printf("NATS warning: failed to get the max pending stats from NATS: %v\n", err)
			continue
		}
		w.stats.Max(statMaxPending, maxBytes)

		drop, err = sub.Dropped()
		if err != nil {
			log.Printf("NATS warning: failed to get the number of dropped message from NATS: %v\n", err)
			continue
		}

		if drop != last {
			w.signalDrop(sub.Subject, drop, last)
		}
		last = drop

		select {
		case <-time.After(time.Second):
		case <-w.stop:
			sub.Unsubscribe()
			return
		}
	}
}

// This goroutine is responsible for monitoring the statistics and
// sending it to the monitoring backend.
func (w *Writer) startStatistician() {
	defer w.wg.Done()

	labels := w.metricsLabels()
	for {
		lines := stats.SnapshotToPrometheus(w.stats.Snapshot(), time.Now(), labels)
		w.nc.Publish(w.c.NATSSubjectMonitor, lines)

		select {
		case <-time.After(3 * time.Second):
		case <-w.stop:
			return
		}
	}
}

func (w *Writer) metricsLabels() map[string]string {
	return map[string]string{
		"component":        "writer",
		"name":             w.c.Name,
		"influxdb_address": w.c.InfluxDBAddress,
		"influxdb_port":    strconv.Itoa(w.c.InfluxDBPort),
		"influxdb_dbname":  w.c.DBName,
	}
}
