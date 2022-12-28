package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/prometheus/tsdb/record"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/backoff"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/grafana/loki/clients/pkg/promtail/api"

	"github.com/grafana/loki/pkg/ingester"
	"github.com/grafana/loki/pkg/util"
	lokiutil "github.com/grafana/loki/pkg/util"
	"github.com/grafana/loki/pkg/util/build"
	"github.com/grafana/loki/pkg/util/wal"
)

const (
	contentType  = "application/x-protobuf"
	maxErrMsgLen = 1024

	// Label reserved to override the tenant ID while processing
	// pipeline stages
	ReservedLabelTenantID = "__tenant_id__"

	LatencyLabel = "filename"
	HostLabel    = "host"
	ClientLabel  = "client"
)

var UserAgent = fmt.Sprintf("promtail/%s", build.Version)

type Metrics struct {
	registerer prometheus.Registerer

	encodedBytes     *prometheus.CounterVec
	sentBytes        *prometheus.CounterVec
	droppedBytes     *prometheus.CounterVec
	sentEntries      *prometheus.CounterVec
	droppedEntries   *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	batchRetries     *prometheus.CounterVec
	countersWithHost []*prometheus.CounterVec
	streamLag        *prometheus.GaugeVec
}

func NewMetrics(reg prometheus.Registerer, streamLagLabels []string) *Metrics {
	m := Metrics{
		registerer: reg,
	}

	m.encodedBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "encoded_bytes_total",
		Help:      "Number of bytes encoded and ready to send.",
	}, []string{HostLabel})
	m.sentBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "sent_bytes_total",
		Help:      "Number of bytes sent.",
	}, []string{HostLabel})
	m.droppedBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "dropped_bytes_total",
		Help:      "Number of bytes dropped because failed to be sent to the ingester after all retries.",
	}, []string{HostLabel})
	m.sentEntries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "sent_entries_total",
		Help:      "Number of log entries sent to the ingester.",
	}, []string{HostLabel})
	m.droppedEntries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "dropped_entries_total",
		Help:      "Number of log entries dropped because failed to be sent to the ingester after all retries.",
	}, []string{HostLabel})
	m.requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "promtail",
		Name:      "request_duration_seconds",
		Help:      "Duration of send requests.",
	}, []string{"status_code", HostLabel})
	m.batchRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "batch_retries_total",
		Help:      "Number of times batches has had to be retried.",
	}, []string{HostLabel})

	m.countersWithHost = []*prometheus.CounterVec{
		m.encodedBytes, m.sentBytes, m.droppedBytes, m.sentEntries, m.droppedEntries,
	}

	streamLagLabelsMerged := []string{HostLabel, ClientLabel}
	streamLagLabelsMerged = append(streamLagLabelsMerged, streamLagLabels...)
	m.streamLag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "promtail",
		Name:      "stream_lag_seconds",
		Help:      "Difference between current time and last batch timestamp for successful sends",
	}, streamLagLabelsMerged)

	if reg != nil {
		m.encodedBytes = mustRegisterOrGet(reg, m.encodedBytes).(*prometheus.CounterVec)
		m.sentBytes = mustRegisterOrGet(reg, m.sentBytes).(*prometheus.CounterVec)
		m.droppedBytes = mustRegisterOrGet(reg, m.droppedBytes).(*prometheus.CounterVec)
		m.sentEntries = mustRegisterOrGet(reg, m.sentEntries).(*prometheus.CounterVec)
		m.droppedEntries = mustRegisterOrGet(reg, m.droppedEntries).(*prometheus.CounterVec)
		m.requestDuration = mustRegisterOrGet(reg, m.requestDuration).(*prometheus.HistogramVec)
		m.batchRetries = mustRegisterOrGet(reg, m.batchRetries).(*prometheus.CounterVec)
		m.streamLag = mustRegisterOrGet(reg, m.streamLag).(*prometheus.GaugeVec)
	}

	return &m
}

func mustRegisterOrGet(reg prometheus.Registerer, c prometheus.Collector) prometheus.Collector {
	if err := reg.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return are.ExistingCollector
		}
		panic(err)
	}
	return c
}

// Client pushes entries to Loki and can be stopped
type Client interface {
	api.EntryHandler
	// Stop goroutine sending batch of entries without retries.
	StopNow()
	Name() string
}

// Client for pushing logs in snappy-compressed protos over HTTP.
type client struct {
	name            string
	metrics         *Metrics
	streamLagLabels []string
	logger          log.Logger
	cfg             Config
	client          *http.Client
	entries         chan api.Entry

	once sync.Once
	wg   sync.WaitGroup

	externalLabels model.LabelSet

	// ctx is used in any upstream calls from the `client`.
	ctx        context.Context
	cancel     context.CancelFunc
	maxStreams int

	wal clientWAL
}

// Tripperware can wrap a roundtripper.
type Tripperware func(http.RoundTripper) http.RoundTripper

// New makes a new Client.
func New(metrics *Metrics, cfg Config, streamLagLabels []string, maxStreams int, logger log.Logger) (Client, error) {
	if cfg.StreamLagLabels.String() != "" {
		return nil, fmt.Errorf("client config stream_lag_labels is deprecated in favour of the config file options block field, and will be ignored: %+v", cfg.StreamLagLabels.String())
	}
	return newClient(metrics, cfg, streamLagLabels, maxStreams, logger)
}

func newClient(metrics *Metrics, cfg Config, streamLagLabels []string, maxStreams int, logger log.Logger) (*client, error) {

	if cfg.URL.URL == nil {
		return nil, errors.New("client needs target URL")
	}

	ctx, cancel := context.WithCancel(context.Background())

	c := &client{
		logger:          log.With(logger, "component", "client", "host", cfg.URL.Host),
		cfg:             cfg,
		entries:         make(chan api.Entry),
		metrics:         metrics,
		streamLagLabels: streamLagLabels,
		name:            asSha256(cfg),

		externalLabels: cfg.ExternalLabels.LabelSet,
		ctx:            ctx,
		cancel:         cancel,
		maxStreams:     maxStreams,
	}
	if cfg.Name != "" {
		c.name = cfg.Name
	}
	c.wal = newClientWAL(c)

	err := cfg.Client.Validate()
	if err != nil {
		return nil, err
	}

	c.client, err = config.NewClientFromConfig(cfg.Client, "promtail", config.WithHTTP2Disabled())
	if err != nil {
		return nil, err
	}

	c.client.Timeout = cfg.Timeout

	// Initialize counters to 0 so the metrics are exported before the first
	// occurrence of incrementing to avoid missing metrics.
	for _, counter := range c.metrics.countersWithHost {
		counter.WithLabelValues(c.cfg.URL.Host).Add(0)
	}

	c.wg.Add(1)

	if cfg.WAL.Enabled {
		go c.runWithWAL()
	} else {
		go c.runSendSide(c.entries)
	}
	return c, nil
}

// NewWithTripperware creates a new Loki client with a custom tripperware.
func NewWithTripperware(metrics *Metrics, cfg Config, streamLagLabels []string, maxStreams int, logger log.Logger, tp Tripperware) (Client, error) {
	c, err := newClient(metrics, cfg, streamLagLabels, maxStreams, logger)
	if err != nil {
		return nil, err
	}

	if tp != nil {
		c.client.Transport = tp(c.client.Transport)
	}

	return c, nil
}

// TODO: can this be turned into an implementation of the pkg/ingester/recovery.go Recoverer interface
// with the current file structure would I need to build a list of all the timestamp/segment files first?
func (c *client) replayWAL() error {
	var recordPool = newRecordPool()

	clientBaseWALDir := path.Join(c.cfg.WAL.Dir, c.name)
	// look for the WAL dir
	_, err := os.Stat(clientBaseWALDir)
	if os.IsNotExist(err) {
		return err
	}
	// get tenant directories for the client, since we could have multiple as a result of the tenant pipeline stage
	// Note: Ignoring errors.
	matches, _ := filepath.Glob(clientBaseWALDir + "/*")
	var tenantDirs []string
	for _, match := range matches {
		f, _ := os.Stat(match)
		if f.IsDir() {
			tenantDirs = append(tenantDirs, match)
		}
	}
	// no wal files
	if len(matches) < 1 {
		return nil
	}
	for _, tenantDir := range tenantDirs {
		tenantID := tenantDir[strings.LastIndex(tenantDir, "/")+1:]
		r, closer, err := wal.NewWalReader(tenantDir, -1)
		if err != nil {
			return err
		}
		defer closer.Close()

		// todo, reduce allocations
		// todo: thepalbi, use correct maxStreams here
		b := newBatch(0)
		seriesRecs := make(map[uint64]model.LabelSet)
		for r.Next() {
			rec := recordPool.GetRecord()
			entry := api.Entry{}
			if err := ingester.DecodeWALRecord(r.Record(), rec); err != nil {
				// this error doesn't need to be fatal, we should maybe just throw out this batch?
				level.Warn(c.logger).Log("msg", "failed to decode a wal record", "err", err)
			}
			for _, series := range rec.Series {
				seriesRecs[uint64(series.Ref)] = util.MapToModelLabelSet(series.Labels.Map())
			}
			for _, samples := range rec.RefEntries {
				if l, ok := seriesRecs[uint64(samples.Ref)]; ok {
					entry.Labels = l
					for _, e := range samples.Entries {
						entry.Entry = e
						// If adding the entry to the batch will increase the size over the max
						// size allowed, we do send the current batch and then create a new one
						if b.sizeBytesAfter(entry) > c.cfg.BatchSize {
							c.sendBatch(tenantID, b)
							// todo: thepalbi why is the WAL deleted here?
							// ahhh it deletes the WAL for that specific batch, not the one being replayed
							//if err := b.wal.Delete(); err != nil {
							//	level.Error(c.logger).Log("msg", "failed to delete WAL", "err", err)
							//}
							new := c.newBatch(tenantID)
							new.replay(entry)
							b = new
							break
						}

						// The max size of the batch isn't reached, so we can add the entry
						b.replay(entry)
					}

				}
			}
		}
		c.sendBatch(tenantID, b)
	}
	return nil
}

func (c *client) runWithWAL() {
	receiveAndWriteToWAL := func() {
		for e := range c.entries {
			e, tenantID := c.processEntry(e)
			// Get WAL, and write entry to it
			w, _ := c.wal.getWAL(tenantID)
			writeEntryToWAL(e, w, tenantID, c.logger)
		}
	}
	go receiveAndWriteToWAL()
	go c.runSendSide(c.wal.Chan())
}

func (c *client) runSendSide(entries chan api.Entry) {
	batches := map[string]*batch{}

	// Given the client handles multiple batches (1 per tenant) and each batch
	// can be created at a different point in time, we look for batches whose
	// max wait time has been reached every 10 times per BatchWait, so that the
	// maximum delay we have sending batches is 10% of the max waiting time.
	// We apply a cap of 10ms to the ticker, to avoid too frequent checks in
	// case the BatchWait is very low.
	minWaitCheckFrequency := 10 * time.Millisecond
	maxWaitCheckFrequency := c.cfg.BatchWait / 10
	if maxWaitCheckFrequency < minWaitCheckFrequency {
		maxWaitCheckFrequency = minWaitCheckFrequency
	}

	maxWaitCheck := time.NewTicker(maxWaitCheckFrequency)

	defer func() {
		maxWaitCheck.Stop()
		// Send all pending batches
		for tenantID, batch := range batches {
			c.sendBatch(tenantID, batch)
		}

		c.wg.Done()
	}()

	for {
		select {
		case e, ok := <-entries:

			if !ok {
				return
			}
			e, tenantID := c.processEntry(e)

			batch, ok := batches[tenantID]

			// If the batch doesn't exist yet, we create a new one with the entry
			if !ok {
				b := c.newBatch(tenantID)
				batches[tenantID] = b
				b.add(e)
				break
			}

			// If adding the entry to the batch will increase the size over the max
			// size allowed, we do send the current batch and then create a new one
			if batch.sizeBytesAfter(e) > c.cfg.BatchSize {
				c.sendBatch(tenantID, batch)
				new := c.newBatch(tenantID)
				new.add(e)
				batches[tenantID] = new
				break
			}

			// The max size of the batch isn't reached, so we can add the entry
			err := batch.add(e)
			if err != nil {
				level.Error(c.logger).Log("msg", "batch add err", "error", err)
				c.metrics.droppedEntries.WithLabelValues(c.cfg.URL.Host).Inc()
				return
			}
		case <-maxWaitCheck.C:
			// todo cut a segment and  read from the wal instead

			// Send all batches whose max wait time has been reached
			for tenantID, batch := range batches {
				if batch.age() < c.cfg.BatchWait {
					continue
				}
				c.sendBatch(tenantID, batch)
				delete(batches, tenantID)
			}
		}
	}
}

func (c *client) newBatch(tenantID string) *batch {
	return newBatch(0)
}

func (c *client) Chan() chan<- api.Entry {
	return c.entries
}

func asSha256(o interface{}) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%v", o)))

	temp := fmt.Sprintf("%x", h.Sum(nil))
	return temp[:6]
}

func (c *client) sendBatch(tenantID string, batch *batch) error {
	buf, entriesCount, err := batch.encode()
	if err != nil {
		level.Error(c.logger).Log("msg", "error encoding batch", "error", err)
		return err
	}
	bufBytes := float64(len(buf))
	c.metrics.encodedBytes.WithLabelValues(c.cfg.URL.Host).Add(bufBytes)

	backoff := backoff.New(c.ctx, c.cfg.BackoffConfig)
	var status int
	for {
		start := time.Now()
		// send uses `timeout` internally, so `context.Background` is good enough.
		status, err = c.send(context.Background(), tenantID, buf)

		c.metrics.requestDuration.WithLabelValues(strconv.Itoa(status), c.cfg.URL.Host).Observe(time.Since(start).Seconds())

		if err == nil {
			c.metrics.sentBytes.WithLabelValues(c.cfg.URL.Host).Add(bufBytes)
			c.metrics.sentEntries.WithLabelValues(c.cfg.URL.Host).Add(float64(entriesCount))
			for _, s := range batch.streams {
				lbls, err := parser.ParseMetric(s.Labels)
				if err != nil {
					// is this possible?
					level.Warn(c.logger).Log("msg", "error converting stream label string to label.Labels, cannot update lagging metric", "error", err)
					return err
				}

				//nolint:staticcheck
				lblSet := make(prometheus.Labels)
				for _, lbl := range c.streamLagLabels {
					// label from streamLagLabels may not be found but we still need an empty value
					// so that the prometheus client library doesn't panic on inconsistent label cardinality
					value := ""
					for i := range lbls {
						if lbls[i].Name == lbl {
							value = lbls[i].Value
						}
					}
					lblSet[lbl] = value
				}

				//nolint:staticcheck
				if lblSet != nil {
					// always set host
					lblSet[HostLabel] = c.cfg.URL.Host
					// also set client name since if we have multiple promtail clients configured we will run into a
					// duplicate metric collected with same labels error when trying to hit the /metrics endpoint
					lblSet[ClientLabel] = c.name
					c.metrics.streamLag.With(lblSet).Set(time.Since(s.Entries[len(s.Entries)-1].Timestamp).Seconds())
				}
			}
			return nil
		}
		// we know err != nil

		// Only retry 429s, 500s and connection-level errors.
		if status > 0 && status != 429 && status/100 != 5 {
			break
		}

		level.Warn(c.logger).Log("msg", "error sending batch, will retry", "status", status, "error", err)
		c.metrics.batchRetries.WithLabelValues(c.cfg.URL.Host).Inc()
		backoff.Wait()

		// Make sure it sends at least once before checking for retry.
		if !backoff.Ongoing() {
			break
		}
	}

	if err != nil {
		level.Error(c.logger).Log("msg", "final error sending batch", "status", status, "error", err)
		c.metrics.droppedBytes.WithLabelValues(c.cfg.URL.Host).Add(bufBytes)
		c.metrics.droppedEntries.WithLabelValues(c.cfg.URL.Host).Add(float64(entriesCount))
	}
	return err
}

func (c *client) send(ctx context.Context, tenantID string, buf []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequest("POST", c.cfg.URL.String(), bytes.NewReader(buf))
	if err != nil {
		return -1, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", UserAgent)

	// If the tenant ID is not empty promtail is running in multi-tenant mode, so
	// we should send it to Loki
	if tenantID != "" {
		req.Header.Set("X-Scope-OrgID", tenantID)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return -1, err
	}
	defer lokiutil.LogError("closing response body", resp.Body.Close)

	if resp.StatusCode/100 != 2 {
		scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxErrMsgLen))
		line := ""
		if scanner.Scan() {
			line = scanner.Text()
		}
		err = fmt.Errorf("server returned HTTP status %s (%d): %s", resp.Status, resp.StatusCode, line)
	}
	return resp.StatusCode, err
}

func (c *client) getTenantID(labels model.LabelSet) string {
	// Check if it has been overridden while processing the pipeline stages
	if value, ok := labels[ReservedLabelTenantID]; ok {
		return string(value)
	}

	// Check if has been specified in the config
	if c.cfg.TenantID != "" {
		return c.cfg.TenantID
	}

	// Defaults to an empty string, which means the X-Scope-OrgID header
	// will not be sent
	return ""
}

// Stop the client.
func (c *client) Stop() {
	c.once.Do(func() { close(c.entries) })
	c.wal.Stop()
	c.wg.Wait()
}

// StopNow stops the client without retries
func (c *client) StopNow() {
	// cancel will stop retrying http requests.
	c.cancel()
	c.Stop()
}

func (c *client) processEntry(e api.Entry) (api.Entry, string) {
	if len(c.externalLabels) > 0 {
		e.Labels = c.externalLabels.Merge(e.Labels)
	}
	tenantID := c.getTenantID(e.Labels)
	return e, tenantID
}

func (c *client) UnregisterLatencyMetric(labels prometheus.Labels) {
	labels[HostLabel] = c.cfg.URL.Host
	c.metrics.streamLag.Delete(labels)
}

func (c *client) Name() string {
	return c.name
}

func (c *client) Sync() error {
	for _, wal := range c.wal.tenantWALs {
		if err := wal.Sync(); err != nil {
			return err
		}
	}
	return nil
}

type clientWAL struct {
	client      *client
	tenantWALs  map[string]WAL
	readChannel chan api.Entry
	watchers    map[string]stoppable
}

type stoppable interface {
	Stop()
}

func newClientWAL(c *client) clientWAL {
	return clientWAL{
		client:      c,
		tenantWALs:  make(map[string]WAL),
		readChannel: make(chan api.Entry),
		watchers:    make(map[string]stoppable),
	}
}

// Chan returns an api.Entry channel where all WAL watchers write to.
func (c *clientWAL) Chan() chan api.Entry {
	return c.readChannel
}

func (c *clientWAL) getWAL(tenant string) (WAL, error) {
	if w, ok := c.tenantWALs[tenant]; ok {
		return w, nil
	}
	// new wal created, start WAL and watcher to read channel
	wal, err := newWAL(c.client.logger, c.client.metrics.registerer, c.client.cfg.WAL, c.client.name, tenant)
	if err != nil {
		level.Error(c.client.logger).Log("msg", "could not start WAL", "err", err)
		// set the wall to noop
		return nil, err
	}
	consumer := newClientConsumer(c.readChannel, c.client.logger, func(b *batch) error {
		return c.client.sendBatch(tenant, b)
	}, wal)
	watcher := NewWALWatcher(wal.Dir(), consumer, c.client.logger)
	watcher.Start()
	c.watchers[tenant] = watcher
	c.tenantWALs[tenant] = wal
	return wal, nil
}

func (c *clientWAL) Stop() {
	for _, watcher := range c.watchers {
		watcher.Stop()
	}
	close(c.readChannel)
}

type sendBatchFunc func(*batch) error

type SegmentDeleter interface {
	DeleteSegment(segmentNum int) error
}

type clientConsumer struct {
	series         map[uint64]model.LabelSet
	pushChannel    chan api.Entry
	logger         log.Logger
	currentBatch   *batch
	sendBatch      sendBatchFunc
	segmentDeleter SegmentDeleter
}

func newClientConsumer(pushChannel chan api.Entry, logger log.Logger, sendBatch sendBatchFunc, segmentDeleter SegmentDeleter) *clientConsumer {
	return &clientConsumer{
		series:         map[uint64]model.LabelSet{},
		pushChannel:    pushChannel,
		logger:         logger,
		currentBatch:   newBatch(0),
		sendBatch:      sendBatch,
		segmentDeleter: segmentDeleter,
	}
}

func (c *clientConsumer) ConsumeSeries(series record.RefSeries) error {
	c.series[uint64(series.Ref)] = util.MapToModelLabelSet(series.Labels.Map())
	return nil
}

func (c *clientConsumer) ConsumeEntries(samples ingester.RefEntries) error {
	var entry api.Entry
	if l, ok := c.series[uint64(samples.Ref)]; ok {
		entry.Labels = l
		for _, e := range samples.Entries {
			entry.Entry = e
			// Using replay since we know the batch needs to be sent once the segment ends
			c.currentBatch.replay(entry)
		}
	} else {
		// if series is not present for sample, just log for now
		level.Debug(c.logger).Log("series for sample not found")
	}
	return nil
}

func (c *clientConsumer) SegmentEnd(segmentNum int) {
	if err := c.sendBatch(c.currentBatch); err == nil {
		// once the batch has been sent, delete segment if no error
		level.Debug(c.logger).Log("msg", "batch sent successfully. Deleting segment", "segmentNum", segmentNum)
		if err := c.segmentDeleter.DeleteSegment(segmentNum); err != nil {
			level.Error(c.logger).Log("msg", "failed to delete segment after sending batch", "segmentNum", segmentNum)
		}
	}
}
