package lists

import (
	"blocky/metrics"
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

const (
	defaultRefreshPeriod = 4 * time.Hour
)

// nolint:gochecknoglobals
var timeout = 60 * time.Second

type ListCacheType int

const (
	BLACKLIST ListCacheType = iota
	WHITELIST
)

type stringCache map[int]string

func (cache stringCache) elementCount() int {
	count := 0

	for k, v := range cache {
		count += len(v) / k
	}

	return count
}

func (cache stringCache) contains(searchString string) bool {
	searchLen := len(searchString)
	searchBucketLen := len(cache[searchLen]) / searchLen
	idx := sort.Search(searchBucketLen, func(i int) bool {
		return cache[searchLen][i*searchLen:i*searchLen+searchLen] >= searchString
	})

	if idx < searchBucketLen {
		return cache[searchLen][idx*searchLen:idx*searchLen+searchLen] == strings.ToLower(searchString)
	}

	return false
}

func (l ListCacheType) String() string {
	names := [...]string{
		"blacklist",
		"whitelist"}

	return names[l]
}

type Matcher interface {
	// matches passed domain name against cached list entries
	Match(domain string, groupsToCheck []string) (found bool, group string)

	// returns current configuration and stats
	Configuration() []string
}

type ListCache struct {
	groupCaches map[string]stringCache
	lock        sync.RWMutex

	groupToLinks  map[string][]string
	refreshPeriod time.Duration

	counter *prometheus.GaugeVec
}

func (b *ListCache) Configuration() (result []string) {
	if b.refreshPeriod > 0 {
		result = append(result, fmt.Sprintf("refresh period: %d minutes", b.refreshPeriod/time.Minute))
	} else {
		result = append(result, "refresh: disabled")
	}

	result = append(result, "group links:")
	for group, links := range b.groupToLinks {
		result = append(result, fmt.Sprintf("  %s:", group))
		for _, link := range links {
			result = append(result, fmt.Sprintf("   - %s", link))
		}
	}

	result = append(result, "group caches:")

	var total int

	for group, cache := range b.groupCaches {
		result = append(result, fmt.Sprintf("  %s: %d entries", group, cache.elementCount()))
		total += cache.elementCount()
	}

	result = append(result, fmt.Sprintf("  TOTAL: %d entries", total))

	return
}

func NewListCache(t ListCacheType, groupToLinks map[string][]string, refreshPeriod int) *ListCache {
	groupCaches := make(map[string]stringCache)

	p := time.Duration(refreshPeriod) * time.Minute
	if refreshPeriod == 0 {
		p = defaultRefreshPeriod
	}

	var counter *prometheus.GaugeVec

	if metrics.IsEnabled() {
		counter = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: fmt.Sprintf("blocky_%s_cache", t),
				Help: "Number of entries in cache",
			}, []string{"group"},
		)

		metrics.RegisterMetric(counter)
	}

	b := &ListCache{
		groupToLinks:  groupToLinks,
		groupCaches:   groupCaches,
		refreshPeriod: p,
		counter:       counter,
	}
	b.refresh()

	go periodicUpdate(b)

	return b
}

// triggers periodical refresh (and download) of list entries
func periodicUpdate(cache *ListCache) {
	if cache.refreshPeriod > 0 {
		ticker := time.NewTicker(cache.refreshPeriod)
		defer ticker.Stop()

		for {
			<-ticker.C
			cache.refresh()
		}
	}
}

func logger() *logrus.Entry {
	return logrus.WithField("prefix", "list_cache")
}

// downloads and reads files with domain names and creates cache for them
func createCacheForGroup(links []string) stringCache {
	cache := make(stringCache)

	keys := make(map[string]struct{})

	var wg sync.WaitGroup

	c := make(chan []string, len(links))

	for _, link := range links {
		wg.Add(1)

		go processFile(link, c, &wg)
	}

	wg.Wait()

	tmp := make(map[int]*strings.Builder)

Loop:
	for {
		select {
		case res := <-c:
			if res == nil {
				return nil
			}
			for _, entry := range res {
				if _, value := keys[entry]; !value {
					keys[entry] = struct{}{}
					if tmp[len(entry)] == nil {
						tmp[len(entry)] = &strings.Builder{}
					}
					tmp[len(entry)].WriteString(entry)
				}
			}
		default:
			close(c)
			break Loop
		}
	}

	for k, v := range tmp {
		chunks := chunks(v.String(), k)
		sort.Strings(chunks)

		cache[k] = strings.Join(chunks, "")

		v.Reset()
	}

	return cache
}

func (b *ListCache) Match(domain string, groupsToCheck []string) (found bool, group string) {
	b.lock.RLock()
	defer b.lock.RUnlock()

	for _, g := range groupsToCheck {
		if b.groupCaches[g].contains(domain) {
			return true, g
		}
	}

	return false, ""
}

func (b *ListCache) refresh() {
	for group, links := range b.groupToLinks {
		cacheForGroup := createCacheForGroup(links)

		if cacheForGroup != nil {
			b.lock.Lock()
			b.groupCaches[group] = cacheForGroup
			b.lock.Unlock()
		} else {
			logger().Warn("Populating of group cache failed, leaving items from last successful download in cache")
		}

		if metrics.IsEnabled() {
			b.counter.WithLabelValues(group).Set(float64(b.groupCaches[group].elementCount()))
		}

		logger().WithFields(logrus.Fields{
			"group":       group,
			"total_count": b.groupCaches[group].elementCount(),
		}).Info("group import finished")
	}
}

func downloadFile(link string) (io.ReadCloser, error) {
	client := http.Client{
		Timeout: timeout,
	}

	var resp *http.Response

	var err error

	logger().WithField("link", link).Info("starting download")

	attempt := 1

	for attempt <= 3 {
		//nolint:bodyclose
		if resp, err = client.Get(link); err == nil {
			if resp.StatusCode == http.StatusOK {
				return resp.Body, nil
			}

			resp.Body.Close()

			return nil, fmt.Errorf("couldn't download url, got status code %d", resp.StatusCode)
		}

		if errNet, ok := err.(net.Error); ok && (errNet.Timeout() || errNet.Temporary()) {
			logger().WithField("link", link).WithField("attempt",
				attempt).Warnf("Temporary network error / Timeout occurred, retrying... %s", errNet)
			time.Sleep(time.Second)
			attempt++
		} else {
			return nil, err
		}
	}

	return nil, err
}

func readFile(file string) (io.ReadCloser, error) {
	logger().WithField("file", file).Info("starting processing of file")
	file = strings.TrimPrefix(file, "file://")

	return os.Open(file)
}

// downloads file (or reads local file) and writes file content as string array in the channel
func processFile(link string, ch chan<- []string, wg *sync.WaitGroup) {
	defer wg.Done()

	result := make([]string, 0)

	var r io.ReadCloser

	var err error

	if strings.HasPrefix(link, "http") {
		r, err = downloadFile(link)
	} else {
		r, err = readFile(link)
	}

	if err != nil {
		logger().Warn("error during file processing: ", err)

		if errNet, ok := err.(net.Error); ok && (errNet.Timeout() || errNet.Temporary()) {
			// put nil to indicate the temporary error
			ch <- nil
			return
		}
		ch <- []string{}

		return
	}
	defer r.Close()

	var count int

	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := scanner.Text()
		// skip comments
		if line := processLine(line); line != "" {
			result = append(result, line)

			count++
		}
	}

	if err := scanner.Err(); err != nil {
		logger().Warn("can't parse file: ", err)
	} else {
		logger().WithFields(logrus.Fields{
			"source": link,
			"count":  count,
		}).Info("file imported")
	}
	ch <- result
}

func chunks(s string, chunkSize int) []string {
	if chunkSize >= len(s) {
		return []string{s}
	}

	var chunks []string

	chunk := make([]rune, chunkSize)
	len := 0

	for _, r := range s {
		chunk[len] = r
		len++

		if len == chunkSize {
			chunks = append(chunks, string(chunk))
			len = 0
		}
	}

	if len > 0 {
		chunks = append(chunks, string(chunk[:len]))
	}

	return chunks
}

// return only first column (see hosts format)
func processLine(line string) string {
	if strings.HasPrefix(line, "#") {
		return ""
	}

	parts := strings.Fields(line)

	if len(parts) > 0 {
		host := parts[len(parts)-1]

		ip := net.ParseIP(host)
		if ip != nil {
			return ip.String()
		}

		return strings.ToLower(host)
	}

	return ""
}
