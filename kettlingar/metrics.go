package kettlingar

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	PublicMetric  bool = false
	PrivateMetric bool = true
)

type MetricsMethods struct{}

type MetricLabels map[string]string

type MetricValue struct {
	Count uint64
	Sum   float64 // For response times
}

type MetricHistogram [20]uint64

type Metrics struct {
	mu         sync.RWMutex
	Help       map[string]string
	Info       map[string]string
	Counts     map[string]uint64
	Guages     map[string]uint64
	Histograms map[string]MetricHistogram
}

func NewMetrics() *Metrics {
	return &Metrics{
		Help:       make(map[string]string),
		Info:       make(map[string]string),
		Counts:     make(map[string]uint64),
		Guages:     make(map[string]uint64),
		Histograms: make(map[string]MetricHistogram),
	}
}

func (d *MetricsMethods) GetDocs() map[string]MethodDesc {
	return map[string]MethodDesc{
		"metrics": {
			Help: "Return internal service metrics",
			Docs: "This endpoint returns the internal service metrics",
		},
	}
}

func (ks *KettlingarService) getMetrics(privPub bool) *Metrics {
	if privPub == PrivateMetric {
		return ks.metricsPriv
	}
	return ks.metrics
}

func keyWithLabels(key string, labels MetricLabels) string {
	if labels == nil || len(labels) < 1 {
		return key
	}
	keys := slices.Collect(maps.Keys(labels))
	slices.Sort(keys)

	lText := ""
	for _, lab := range keys {
		lText += ", " + lab + "=\"" + labels[lab] + "\""
	}
	return key + "{" + lText[2:] + "}"
}

func (ks *KettlingarService) MetricsInfo(key, val string, public bool, labels MetricLabels) {
	k := keyWithLabels(key, labels)
	m := ks.getMetrics(public)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Info[k] = val
}

func (ks *KettlingarService) MetricsGuage(key string, val uint64, public bool, labels MetricLabels) {
	k := keyWithLabels(key, labels)
	m := ks.getMetrics(public)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Guages[k] = val
}

func (ks *KettlingarService) MetricsCount(key string, val uint64, public bool, labels MetricLabels) {
	k := keyWithLabels(key, labels)
	m := ks.getMetrics(public)
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.Counts[k]; ok {
		val += cur
	}
	m.Counts[k] = val
}

func (ks *KettlingarService) MetricsSample(key string, val uint64, public bool, labels MetricLabels) {
	bucket := int(math.Ceil(math.Log2(float64(val)))) - 3

	k := keyWithLabels(key, labels)
	m := ks.getMetrics(public)
	m.mu.Lock()
	defer m.mu.Unlock()

	histogram, ok := m.Histograms[k]
	if !ok {
		// Slot 0 is the creation timestamp
		histogram = MetricHistogram{uint64(time.Now().Unix())}
	}
	// Slot 1 is the real sum
	histogram[1] += val

	if bucket < 2 {
		bucket = 1.0
	} else if bucket >= len(m.Histograms[k]) {
		bucket = len(m.Histograms[k]) - 1
	}
	for i := int(bucket); i < len(m.Histograms[k]); i++ {
		histogram[i] += 1
	}
	m.Histograms[k] = histogram
}

type MetricsResponse struct {
	Timestamp   time.Time `msgpack:"timestamp" json:"timestamp"`
	PubMetrics  Metrics   `msgpack:"public_metrics" json:"public_metrics"`
	PrivMetrics Metrics   `msgpack:"private_metrics" json:"private_metrics"`
}

func (d *MetricsMethods) PublicApiMetrics(ri *RequestInfo) MetricsResponse {
	response := MetricsResponse{
		Timestamp:  time.Now(),
		PubMetrics: *ri.Service.metrics,
	}
	if ri.IsAuthed {
		response.PrivMetrics = *ri.Service.metricsPriv
	}
	return response
}

func (mr *MetricsResponse) Render(mimeType string) (string, []byte) {
	switch mimeType {
	case "application/openmetrics-text", "text/plain", "text":
		return "application/openmetrics-text; version=1.0.0", mr.RenderOpenMetrics()
	}
	return "application/openmetrics-text", nil
}

func addLabel(key, k, v string) string {
	klen := len(key) - 1
	if key[klen:] == "}" {
		return fmt.Sprintf("%s, %s=\"%s\"}", key[:klen], k, v)
	}
	return fmt.Sprintf("%s{%s=\"%s\"}", key, k, v)
}

func (mr *MetricsResponse) RenderOpenMetrics() []byte {
	all := make(map[string]string)
	aTypes := make(map[string]string)
	help := make(map[string]string)
	counts := make(map[string]uint64)
	guages := make(map[string]uint64)
	histogs := make(map[string]MetricHistogram)

	mr.PubMetrics.mu.Lock()
	maps.Copy(all, mr.PubMetrics.Info)
	maps.Copy(help, mr.PubMetrics.Help)
	maps.Copy(counts, mr.PubMetrics.Counts)
	maps.Copy(guages, mr.PubMetrics.Guages)
	maps.Copy(histogs, mr.PubMetrics.Histograms) // May be a shallow copy?
	mr.PubMetrics.mu.Unlock()

	mr.PrivMetrics.mu.Lock()
	maps.Copy(all, mr.PrivMetrics.Info)
	maps.Copy(help, mr.PrivMetrics.Help)
	maps.Copy(counts, mr.PrivMetrics.Counts)
	maps.Copy(guages, mr.PrivMetrics.Guages)
	maps.Copy(histogs, mr.PrivMetrics.Histograms) // May be a shallow copy?
	mr.PrivMetrics.mu.Unlock()

	for k, v := range counts {
		aTypes[k] = "counter"
		all[k] = fmt.Sprintf("%d", v)
	}
	for k, v := range guages {
		aTypes[k] = "guage"
		all[k] = fmt.Sprintf("%d", v)
	}
	for k := range histogs {
		aTypes[k] = "histogram"
		all[k] = ""
	}

	keys := slices.Collect(maps.Keys(all))
	slices.Sort(keys)

	output := make([]string, 0)
	lastMetric := ""
	for _, k := range keys {
		vType := "info"
		if t, ok := aTypes[k]; ok {
			vType = t
		}

		baseName, _, _ := strings.Cut(k, "{")
		if baseName != lastMetric {
			vHelp := "Generic Metrics: " + vType
			if t, ok := help[baseName]; ok {
				vHelp = t
			}
			output = append(output, fmt.Sprintf(
				"\n# HELP %s %s\n# TYPE %s %s\n",
				baseName, vHelp, baseName, vType))
		}
		switch vType {
		case "info":
			k = addLabel(k, "info", all[k])
			output = append(output, fmt.Sprintf("%s 1\n", k))
		case "histogram":
			output = append(output, renderHistogram(baseName, k, histogs[k]))
		default:
			output = append(output, fmt.Sprintf("%s %s\n", k, all[k]))
		}
		lastMetric = baseName
	}

	output = append(output, "\n# EOF\n")
	output[0] = strings.TrimSpace(output[0]) + "\n"
	return []byte(strings.Join(output, ""))
}

func renderHistogram(baseName, key string, hist MetricHistogram) string {
	output := make([]string, 0)
	labels := key[len(baseName):]
	first := true
	last := len(hist) - 1

	for i := 2; i <= last; i++ {
		if hist[i] == 0 || ((hist[i] == hist[i-1]) && (hist[i] == hist[last]) && (i != last) && !first) {
			continue
		}
		le := "+Inf"
		bucket := "_bucket"
		if i != last {
			le = fmt.Sprintf("%.0f", math.Pow(2, float64(i+3)))
		}
		bk := addLabel(baseName+bucket+labels, "le", le)
		output = append(output, fmt.Sprintf("%s %d\n", bk, hist[i]))
		if le == "+Inf" {
			bk := baseName + "_count" + labels
			output = append(output, fmt.Sprintf("%s %d\n", bk, hist[i]))
		}
		first = false
	}

	sum := hist[1]
	output = append(output, fmt.Sprintf("%s_sum%s %d\n", baseName, labels, sum))

	created := hist[0]
	output = append(output, fmt.Sprintf("%s_created%s %d\n", baseName, labels, created))

	return strings.Join(output, "")
}
