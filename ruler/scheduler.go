package ruler

import (
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"golang.org/x/net/context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/prometheus/rules"

	"github.com/weaveworks/common/instrument"
	"github.com/weaveworks/cortex/configs"
)

const (
	// Backoff for loading initial configuration set.
	minBackoff = 100 * time.Millisecond
	maxBackoff = 2 * time.Second
)

var (
	totalConfigs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "cortex",
		Name:      "configs",
		Help:      "How many configs the scheduler knows about.",
	})
	configsRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "configs_request_duration_seconds",
		Help:      "Time spent requesting configs.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"operation", "status_code"})
)

func init() {
	prometheus.MustRegister(configsRequestDuration)
	prometheus.MustRegister(totalConfigs)
}

type workItem struct {
	userID    string
	rules     []rules.Rule
	scheduled time.Time
}

// Key implements ScheduledItem
func (w workItem) Key() string {
	return w.userID
}

// Scheduled implements ScheduledItem
func (w workItem) Scheduled() time.Time {
	return w.scheduled
}

// Defer returns a copy of this work item, rescheduled to a later time.
func (w workItem) Defer(interval time.Duration) workItem {
	return workItem{w.userID, w.rules, w.scheduled.Add(interval)}
}

type scheduler struct {
	configsAPI         configs.API // XXX: Maybe make this an interface ConfigSource or similar.
	evaluationInterval time.Duration
	q                  *SchedulingQueue

	// All the configurations that we have. Only used for instrumentation.
	cfgs map[string]configs.CortexConfig

	pollInterval time.Duration

	latestConfig configs.ConfigID
	latestMutex  sync.RWMutex

	stop chan struct{}
	done chan struct{}
}

// newScheduler makes a new scheduler.
func newScheduler(configsAPI configs.API, evaluationInterval, pollInterval time.Duration) scheduler {
	return scheduler{
		configsAPI:         configsAPI,
		evaluationInterval: evaluationInterval,
		pollInterval:       pollInterval,
		q:                  NewSchedulingQueue(clockwork.NewRealClock()),
		cfgs:               map[string]configs.CortexConfig{},

		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Run polls the source of configurations for changes.
func (s *scheduler) Run() {
	log.Debugf("Scheduler started")
	defer close(s.done)
	// Load initial set of all configurations before polling for new ones.
	s.addNewConfigs(time.Now(), s.loadAllConfigs())
	ticker := time.NewTicker(s.pollInterval)
	for {
		select {
		case now := <-ticker.C:
			err := s.updateConfigs(now)
			if err != nil {
				log.Warnf("Scheduler: error updating configs: %v", err)
			}
		case <-s.stop:
			ticker.Stop()
			return
		}
	}
}

func (s *scheduler) Stop() {
	close(s.stop)
	s.q.Close()
	<-s.done
	log.Debugf("Scheduler stopped")
}

// Load the full set of configurations from the server, retrying with backoff
// until we can get them.
func (s *scheduler) loadAllConfigs() map[string]configs.CortexConfigView {
	backoff := minBackoff
	for {
		cfgs, err := s.poll()
		if err == nil {
			log.Debugf("Scheduler: found %d configurations in initial load", len(cfgs))
			return cfgs
		}
		log.Warnf("Scheduler: error fetching all configurations, backing off: %v", err)
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (s *scheduler) updateConfigs(now time.Time) error {
	cfgs, err := s.poll()
	if err != nil {
		return err
	}
	s.addNewConfigs(now, cfgs)
	return nil
}

// poll the configuration server. Not re-entrant.
func (s *scheduler) poll() (map[string]configs.CortexConfigView, error) {
	configID := s.latestConfig
	var cfgs *configs.CortexConfigsResponse
	err := instrument.TimeRequestHistogram(context.Background(), "Configs.GetOrgConfigs", configsRequestDuration, func(_ context.Context) error {
		var err error
		cfgs, err = s.configsAPI.GetOrgConfigs(configID)
		return err
	})
	if err != nil {
		log.Warnf("Scheduler: configs server poll failed: %v", err)
		return nil, err
	}
	s.latestMutex.Lock()
	s.latestConfig = cfgs.GetLatestConfigID()
	s.latestMutex.Unlock()
	return cfgs.Configs, nil
}

func (s *scheduler) addNewConfigs(now time.Time, cfgs map[string]configs.CortexConfigView) {
	// TODO: instrument how many configs we have, both valid & invalid.
	log.Debugf("Adding %d configurations", len(cfgs))
	for userID, config := range cfgs {
		rules, err := config.Config.GetRules()
		if err != nil {
			// XXX: This means that if a user has a working configuration and
			// they submit a broken one, we'll keep processing the last known
			// working configuration, and they'll never know.
			// TODO: Provide a way of deleting / cancelling recording rules.
			log.Warnf("Scheduler: invalid Cortex configuration for %v: %v", userID, err)
			continue
		}

		s.addWorkItem(workItem{userID, rules, now})
		s.cfgs[userID] = config.Config
	}
	totalConfigs.Set(float64(len(s.cfgs)))
}

func (s *scheduler) addWorkItem(i workItem) {
	// The queue is keyed by user ID, so items for existing user IDs will be replaced.
	s.q.Enqueue(i)
	log.Debugf("Scheduler: work item added: %v", i)
}

// Get the next scheduled work item, blocking if none.
//
// Call `workItemDone` on the returned item to indicate that it is ready to be
// rescheduled.
func (s *scheduler) nextWorkItem() *workItem {
	log.Debugf("Scheduler: work item requested. Pending...")
	// TODO: We are blocking here on the second Dequeue event. Write more
	// tests for the scheduling queue.
	op := s.q.Dequeue()
	if op == nil {
		log.Infof("Queue closed. No more work items.")
		return nil
	}
	item := op.(workItem)
	log.Debugf("Scheduler: work item granted: %v", item)
	return &item
}

// workItemDone marks the given item as being ready to be rescheduled.
func (s *scheduler) workItemDone(i workItem) {
	next := i.Defer(s.evaluationInterval)
	log.Debugf("Scheduler: work item %v rescheduled for %v", i, next.scheduled.Format("2006-01-02 15:04:05"))
	s.addWorkItem(next)
}
