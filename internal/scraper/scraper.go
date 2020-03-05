package scraper

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-logfmt/logfmt"
	"github.com/grafana/worldping-blackbox-sidecar/internal/pkg/pb/logproto"
	"github.com/grafana/worldping-blackbox-sidecar/internal/pkg/pb/prompb"
	"github.com/grafana/worldping-blackbox-sidecar/internal/pkg/pb/worldping"
	"github.com/grafana/worldping-blackbox-sidecar/internal/pusher"
	bbeconfig "github.com/prometheus/blackbox_exporter/config"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

type Scraper struct {
	publishCh  chan<- pusher.Payload
	probeName  string
	checkName  string
	provider   url.URL // provider is the BBE URL
	endpoint   string  // endpoint is the thing to be checked (hostname, URL, ...)
	logger     logger
	check      worldping.Check
	bbeModule  *bbeconfig.Module
	moduleName string
}

type logger interface {
	Printf(format string, v ...interface{})
}

type TimeSeries = []prompb.TimeSeries
type Streams = []logproto.Stream

type probeData struct {
	ts      TimeSeries
	streams Streams
}

func (d *probeData) Metrics() TimeSeries {
	return d.ts
}

func (d *probeData) Streams() Streams {
	return d.streams
}

func New(check worldping.Check, publishCh chan<- pusher.Payload, probeName string, probeURL url.URL, logger logger) (*Scraper, error) {
	var (
		target     string
		checkName  string
		moduleName string
		bbeModule  bbeconfig.Module
	)

	bbeModule.Timeout = time.Duration(check.Timeout) * time.Millisecond

	ipVersionToIpProtocol := func(v worldping.IpVersion) (string, bool) {
		switch v {
		case worldping.IpVersion_V4:
			// preferred_ip_protocol = ip4
			// ip_protocol_fallback = false
			return "ip4", false
		case worldping.IpVersion_V6:
			// preferred_ip_protocol = ip6
			// ip_protocol_fallback = false
			return "ip6", false
		case worldping.IpVersion_Any:
			// preferred_ip_protocol = ip6
			// ip_protocol_fallback = true
			return "ip6", true
		}

		return "", false
	}

	// Map the change to a blackbox exporter module
	if check.Settings.Ping != nil {
		bbeModule.Prober = "icmp"
		checkName = "ping"

		bbeModule.ICMP.IPProtocol, bbeModule.ICMP.IPProtocolFallback = ipVersionToIpProtocol(check.Settings.Ping.IpVersion)

		moduleName = fmt.Sprintf("%s_%s_%d", bbeModule.Prober, bbeModule.ICMP.IPProtocol, check.Id)

		target = check.Settings.Ping.Hostname
	} else if check.Settings.Http != nil {
		bbeModule.Prober = "http"
		checkName = "http"

		bbeModule.HTTP.IPProtocol, bbeModule.HTTP.IPProtocolFallback = ipVersionToIpProtocol(check.Settings.Http.IpVersion)

		bbeModule.HTTP.Body = check.Settings.Http.Body

		bbeModule.HTTP.Method = check.Settings.Http.Method.String()

		for _, header := range check.Settings.Http.Headers {
			parts := strings.SplitN(header, ":", 2)
			var value string
			if len(parts) == 2 {
				value = strings.TrimLeft(parts[1], " ")
			}
			bbeModule.HTTP.Headers[parts[0]] = value
		}

		moduleName = fmt.Sprintf("%s_%s_%d", bbeModule.Prober, bbeModule.HTTP.IPProtocol, check.Id)

		target = check.Settings.Http.Url
	} else if check.Settings.Dns != nil {
		checkName = "dns"

		// BBE dns_probe actually tests the DNS server, so we
		// need to pass the query (e.g. www.grafana.com) as part
		// of the configuration and the server as the target
		// parameter.
		bbeModule.DNS.QueryName = check.Settings.Dns.Name
		bbeModule.DNS.QueryType = check.Settings.Dns.RecordType.String()
		bbeModule.DNS.TransportProtocol = "udp"

		bbeModule.DNS.IPProtocol, bbeModule.DNS.IPProtocolFallback = ipVersionToIpProtocol(check.Settings.Dns.IpVersion)

		target = check.Settings.Dns.Server
	} else {
		return nil, fmt.Errorf("unsupported change")
	}

	q := probeURL.Query()
	q.Set("target", target)
	q.Set("module", moduleName)
	probeURL.RawQuery = q.Encode()

	return &Scraper{
		publishCh:  publishCh,
		probeName:  probeName,
		checkName:  checkName,
		provider:   probeURL,
		endpoint:   target,
		logger:     logger,
		check:      check,
		bbeModule:  &bbeModule,
		moduleName: moduleName,
	}, nil
}

var errCheckFailed = errors.New("probe failed")

type checkStateMachine struct {
	passes    int
	failures  int
	threshold int
}

func (sm *checkStateMachine) fail(cb func()) {
	wasFailing := sm.isFailing()
	sm.passes = 0
	sm.failures++
	isFailing := sm.isFailing()

	if isFailing != wasFailing {
		cb()
	}
}

func (sm *checkStateMachine) pass(cb func()) {
	wasPassing := sm.isPassing()
	sm.passes++
	sm.failures = 0
	isPassing := sm.isPassing()

	if isPassing != wasPassing {
		cb()
	}
}

func (sm checkStateMachine) isPassing() bool {
	return sm.passes > sm.threshold
}

func (sm checkStateMachine) isFailing() bool {
	return sm.failures > sm.threshold
}

func (s Scraper) Run(ctx context.Context) {
	s.logger.Printf(`msg="starting scraper" probe=%s endpoint=%s provider=%s`, s.probeName, s.endpoint, s.provider.String())

	// TODO(mem): keep count of the number of successive errors and
	// collect logs if threshold is reached.

	var sm checkStateMachine

	scrape := func(ctx context.Context, t time.Time) {
		payload, err := s.collectData(ctx, t)

		switch {
		case errors.Is(err, errCheckFailed):
			sm.fail(func() {
				s.logger.Printf(`msg="check entered FAIL state" probe=%s endpoint=%s provider=%s`, s.probeName, s.endpoint, s.provider.String())
			})

		case err != nil:
			s.logger.Printf(`msg="error collecting data" probe=%s endpoint=%s provider=%s err="%s"`, s.probeName, s.endpoint, s.provider.String(), err)
			return

		default:
			sm.pass(func() {
				s.logger.Printf(`msg="check entered PASS state" probe=%s endpoint=%s provider=%s`, s.probeName, s.endpoint, s.provider.String())
			})
		}

		if payload != nil {
			s.publishCh <- payload
		}
	}

	tickWithOffset(ctx, scrape, s.check.Offset, s.check.Frequency)
}

func tickWithOffset(ctx context.Context, f func(context.Context, time.Time), offset, period int64) {
	timer := time.NewTimer(time.Duration(offset) * time.Millisecond)

	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return

	case t := <-timer.C:
		f(ctx, t)
	}

	ticker := time.NewTicker(time.Duration(period) * time.Millisecond)

	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			return

		case t := <-ticker.C:
			f(ctx, t)
		}

	}
}

func (s *Scraper) Update(check worldping.Check) {
	s.check = check

	// XXX(mem): restart scraper

	if !check.Enabled {
		// XXX(mem): this scraper must be running (current
		// s.check.Enabled == true); stop it in that case.
		s.logger.Printf("check is disabled for probe=%s endpoint=%s provider=%s", s.probeName, s.endpoint, s.provider)
		return
	}

	// XXX(mem): this needs to change to check for existing queries
	// and to handle enabling / disabling of checks
}

func (s *Scraper) Delete() {
	// XXX(mem): stop the running scraper
}

func (s *Scraper) GetModuleName() string {
	return s.moduleName
}

func (s *Scraper) GetModuleConfig() interface{} {
	return s.bbeModule
}

func (s Scraper) collectData(ctx context.Context, t time.Time) (*probeData, error) {
	u := s.provider
	q := u.Query()
	// this is needed in order to obtain the logs alongside the metrics
	q.Add("debug", "true")
	u.RawQuery = q.Encode()
	target := u.String()

	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		err = fmt.Errorf(`msg="creating new request" probe=%s endpoint=%s target=%s err="%w"`, s.probeName, s.endpoint, target, err)
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		err = fmt.Errorf(`msg="requesting data from" probe=%s endpoint=%s target=%s err="%w"`, s.probeName, s.endpoint, target, err)
		return nil, err
	}

	defer func() {
		// drain body
		_, _ = io.Copy(ioutil.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	metrics, logs, err := extractMetricsAndLogs(resp.Body)
	if err != nil {
		err = fmt.Errorf(`msg="extracting data from blackbox-exporter" probe=%s endpoint=%s target=%s err="%w"`, s.probeName, s.endpoint, target, err)
		return nil, err
	}

	// TODO(mem): this is constant for the scraper, move this
	// outside this function?
	baseLabels := []labelPair{
		{name: "check_id", value: strconv.FormatInt(s.check.Id, 10)},
		{name: "check_name", value: s.checkName},
		{name: "endpoint", value: s.endpoint},
		{name: "probe", value: s.probeName},
	}

	for _, l := range s.check.Labels {
		baseLabels = append(baseLabels, labelPair{name: "label_" + l.Name, value: l.Value})
	}

	streams := s.extractLogs(t, logs, baseLabels)

	ts, err := s.extractTimeseries(t, metrics, baseLabels)

	return &probeData{ts: ts, streams: streams}, err
}

func (s Scraper) extractLogs(t time.Time, logs []byte, baseLabels []labelPair) Streams {
	var streams Streams
	var line strings.Builder

	dec := logfmt.NewDecoder(bytes.NewReader(logs))

	labels := make([]labelPair, 0, len(baseLabels))

RECORD:
	for dec.ScanRecord() {
		var t time.Time

		line.Reset()

		enc := logfmt.NewEncoder(&line)

		labels = labels[:0]
		labels = append(labels, baseLabels...)

		for dec.ScanKeyval() {
			value := dec.Value()

			switch key := dec.Key(); string(key) {
			case "ts":
				var err error
				t, err = time.Parse(time.RFC3339Nano, string(value))
				if err != nil {
					// We should never hit this as the timestamp string in the log should be valid.
					// Without a timestamp we cannot do anything. And we cannot use something like
					// time.Now() because that would mess up other entries
					s.logger.Printf(`Invalid timestamp "%s" scanning logs: %s:`, string(value), err)
					continue RECORD
				}

			case "caller", "module":
				// skip

			case "level":
				// this has to be tranlated to a label
				labels = append(labels, labelPair{name: "level", value: string(value)})

			default:
				if err := enc.EncodeKeyval(key, value); err != nil {
					// We should never hit this because all the entries are valid.
					s.logger.Printf(`Invalid entry "%s: %s" scanning logs: %s:`, string(key), string(value), err)
					continue RECORD
				}
			}
		}

		if err := enc.EndRecord(); err != nil {
			s.logger.Printf(`Error reencoding logs: %s:`, err)
		}

		// this is creating one stream per log line because the labels might have to change between lines (level
		// is not going to be the same).
		streams = append(streams, logproto.Stream{
			Labels: fmtLabels(labels),
			Entries: []logproto.Entry{
				{
					Timestamp: t,
					Line:      line.String(),
				},
			},
		})
	}

	if err := dec.Err(); err != nil {
		s.logger.Printf("error decoding logs: %s", err)
	}

	return streams
}

func (s Scraper) extractTimeseries(t time.Time, metrics []byte, baseLabels []labelPair) (TimeSeries, error) {
	// XXX(mem): the following is needed in order to derive the
	// correct format from the response headers, but since we are
	// passing debug=true, we loose access to that.
	//
	// format := expfmt.ResponseFormat(resp.Header)
	//
	// Instead hard-code the format to be plain text.
	format := expfmt.FmtText

	dec := expfmt.NewDecoder(bytes.NewReader(metrics), format)

	ts := make([]prompb.TimeSeries, 0)

	metricLabels := make([]*prompb.Label, 0, len(baseLabels))
	for _, label := range baseLabels {
		metricLabels = append(metricLabels, &prompb.Label{Name: label.name, Value: label.value})
	}

	for {
		var mf dto.MetricFamily

		switch err := dec.Decode(&mf); err {
		case nil:
			// got metrics
			mName := mf.GetName()
			mType := mf.GetType()
			isProbeSuccess := mName == "probe_success" && mType == dto.MetricType_GAUGE

			for _, m := range mf.GetMetric() {
				if isProbeSuccess && m.GetGauge().GetValue() == 0 {
					ts = appendDtoToTimeseries(nil, t, mName, metricLabels, mType, m)

					err := fmt.Errorf(`msg="check failed" check_name=%s probe=%s endpoint=%s err="%w"`, s.checkName, s.probeName, s.endpoint, errCheckFailed)
					return ts, err
				}

				ts = appendDtoToTimeseries(ts, t, mName, metricLabels, mType, m)
			}

		case io.EOF:
			return ts, nil

		default:
			return nil, fmt.Errorf(`msg="decoding results from blackbox-exporter" probe=%s endpoint=%s err="%w"`, s.probeName, s.endpoint, err)
		}
	}
}

func makeTimeseries(t time.Time, value float64, labels ...*prompb.Label) prompb.TimeSeries {
	var ts prompb.TimeSeries

	ts.Labels = make([]*prompb.Label, len(labels))
	copy(ts.Labels, labels)

	ts.Samples = []prompb.Sample{
		{Timestamp: t.UnixNano() / 1e6, Value: value},
	}

	return ts
}

func appendDtoToTimeseries(ts []prompb.TimeSeries, t time.Time, mName string, baseLabels []*prompb.Label, mType dto.MetricType, metric *dto.Metric) []prompb.TimeSeries {
	ml := metric.GetLabel()

	labels := make([]*prompb.Label, 0, 1+len(baseLabels)+len(ml))
	labels = append(labels, &prompb.Label{Name: "__name__", Value: mName})
	labels = append(labels, baseLabels...)
	for _, l := range ml {
		labels = append(labels, &prompb.Label{Name: *(l.Name), Value: *(l.Value)})
	}

	switch mType {
	case dto.MetricType_COUNTER:
		if v := metric.GetCounter(); v != nil && v.Value != nil {
			ts = append(ts, makeTimeseries(t, *v.Value, labels...))
		}

	case dto.MetricType_GAUGE:
		if v := metric.GetGauge(); v != nil && v.Value != nil {
			ts = append(ts, makeTimeseries(t, *v.Value, labels...))
		}

	case dto.MetricType_UNTYPED:
		if v := metric.GetUntyped(); v != nil && v.Value != nil {
			ts = append(ts, makeTimeseries(t, *v.Value, labels...))
		}

	case dto.MetricType_SUMMARY:
		if s := metric.GetSummary(); s != nil {
			if q := s.GetQuantile(); q != nil {
				sLabels := make([]*prompb.Label, len(labels))
				copy(sLabels, labels)

				sLabels[0] = &prompb.Label{Name: "__name__", Value: mName + "_sum"}
				ts = append(ts, makeTimeseries(t, s.GetSampleSum(), sLabels...))

				sLabels[0] = &prompb.Label{Name: "__name__", Value: mName + "_count"}
				ts = append(ts, makeTimeseries(t, float64(s.GetSampleCount()), sLabels...))

				sLabels = make([]*prompb.Label, len(labels)+1)
				copy(sLabels, labels)

				for _, v := range q {
					sLabels[len(sLabels)-1] = &prompb.Label{
						Name:  "quantile",
						Value: strconv.FormatFloat(v.GetQuantile(), 'G', -1, 64),
					}
					ts = append(ts, makeTimeseries(t, v.GetValue(), sLabels...))
				}
			}
		}

	case dto.MetricType_HISTOGRAM:
		if h := metric.GetHistogram(); h != nil {
			if b := h.GetBucket(); b != nil {
				hLabels := make([]*prompb.Label, len(labels))
				copy(hLabels, labels)

				hLabels[0] = &prompb.Label{Name: "__name__", Value: mName + "_sum"}
				ts = append(ts, makeTimeseries(t, h.GetSampleSum(), hLabels...))

				hLabels[0] = &prompb.Label{Name: "__name__", Value: mName + "_count"}
				ts = append(ts, makeTimeseries(t, float64(h.GetSampleCount()), hLabels...))

				hLabels = make([]*prompb.Label, len(labels)+1)
				copy(hLabels, labels)

				for _, v := range b {
					hLabels[len(hLabels)-1] = &prompb.Label{
						Name:  "le",
						Value: strconv.FormatFloat(v.GetUpperBound(), 'G', -1, 64),
					}
					ts = append(ts, makeTimeseries(t, float64(v.GetCumulativeCount()), hLabels...))
				}
			}
		}
	}

	return ts
}

func extractMetricsAndLogs(r io.Reader) ([]byte, []byte, error) {
	type extractorState int

	const (
		stateLookingForHeader extractorState = iota
		stateInLogs
		stateInMetrics
	)

	var (
		state   extractorState
		metrics bytes.Buffer
		logs    bytes.Buffer
		cur     *bytes.Buffer
	)

	s := bufio.NewScanner(r)

SCAN:
	for s.Scan() {
		switch state {
		case stateLookingForHeader:
			switch text := s.Text(); text {
			case "Logs for the probe:":
				state = stateInLogs
				cur = &logs

			case "Metrics that would have been returned:":
				state = stateInMetrics
				cur = &metrics
			}

		case stateInLogs, stateInMetrics:
			// first blank line ends the data and goes back
			// to searching for the next header
			if s.Text() == "" {
				// we break out early if we have both
				// logs and metrics
				if logs.Len() > 0 && metrics.Len() > 0 {
					break SCAN
				}
				state = stateLookingForHeader
				continue
			}

			if _, err := cur.Write(s.Bytes()); err != nil {
				return nil, nil, err
			}

			if _, err := cur.WriteRune('\n'); err != nil {
				return nil, nil, err
			}
		}
	}

	if err := s.Err(); err != nil {
		return nil, nil, err
	}

	return metrics.Bytes(), logs.Bytes(), nil
}

type labelPair struct {
	name  string
	value string
}

func fmtLabels(labels []labelPair) string {
	if len(labels) == 0 {
		return ""
	}

	var s strings.Builder

	// these calls do not produce errors, the errors are required to
	// satisfy interfaces
	_, _ = s.WriteRune('{')

	for i, pair := range labels {
		if i > 0 {
			_, _ = s.WriteRune(',')
		}
		_, _ = s.WriteString(pair.name)
		_, _ = s.WriteRune('=')
		_, _ = s.WriteRune('"')
		_, _ = s.WriteString(pair.value)
		_, _ = s.WriteRune('"')
	}

	_, _ = s.WriteRune('}')

	return s.String()
}
