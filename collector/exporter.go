package collector

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

// Metric name parts.
const (
	// Subsystem(s).
	exporter = "exporter"
)

// SQL Queries.
const (
	sessionSettingsQuery = `SET SESSION log_slow_filter = 'tmp_table_on_disk,filesort_on_disk'`
	upQuery              = `SELECT 1`
)

// Metric descriptors.
var (
	db                 *sql.DB
	inited             int32 = 0
	mtx                      = &sync.Mutex{}
	scrapeDurationDesc       = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, exporter, "collector_duration_seconds"),
		"Collector time duration.",
		[]string{"collector"}, nil,
	)
)

// Collect defines which metrics we should collect
type Collect struct {
	SlowLogFilter        bool
	Processlist          bool
	TableSchema          bool
	InnodbTablespaces    bool
	InnodbMetrics        bool
	GlobalStatus         bool
	GlobalVariables      bool
	SlaveStatus          bool
	AutoIncrementColumns bool
	BinlogSize           bool
	PerfTableIOWaits     bool
	PerfIndexIOWaits     bool
	PerfTableLockWaits   bool
	PerfEventsStatements bool
	PerfEventsWaits      bool
	PerfFileEvents       bool
	PerfFileInstances    bool
	UserStat             bool
	ClientStat           bool
	TableStat            bool
	QueryResponseTime    bool
	EngineTokudbStatus   bool
	EngineInnodbStatus   bool
	Heartbeat            bool
	HeartbeatDatabase    string
	HeartbeatTable       string
	MaxMySQLConns        int
}

// Exporter collects MySQL metrics. It implements prometheus.Collector.
type Exporter struct {
	dsn          string
	collect      Collect
	error        prometheus.Gauge
	totalScrapes prometheus.Counter
	scrapeErrors *prometheus.CounterVec
	mysqldUp     prometheus.Gauge
}

// New returns a new MySQL exporter for the provided DSN.
func New(dsn string, collect Collect) *Exporter {
	return &Exporter{
		dsn:     dsn,
		collect: collect,
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "scrapes_total",
			Help:      "Total number of times MySQL was scraped for metrics.",
		}),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "scrape_errors_total",
			Help:      "Total number of times an error occurred scraping a MySQL.",
		}, []string{"collector"}),
		error: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "last_scrape_error",
			Help:      "Whether the last scrape of metrics from MySQL resulted in an error (1 for error, 0 for success).",
		}),
		mysqldUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Whether the MySQL server is up.",
		}),
	}
}

// Describe implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	// We cannot know in advance what metrics the exporter will generate
	// from MySQL. So we use the poor man's describe method: Run a collect
	// and send the descriptors of all the collected metrics. The problem
	// here is that we need to connect to the MySQL DB. If it is currently
	// unavailable, the descriptors will be incomplete. Since this is a
	// stand-alone exporter and not used as a library within other code
	// implementing additional metrics, the worst that can happen is that we
	// don't detect inconsistent metrics created by this exporter
	// itself. Also, a change in the monitored MySQL instance may change the
	// exported metrics during the runtime of the exporter.

	metricCh := make(chan prometheus.Metric)
	doneCh := make(chan struct{})

	go func() {
		for m := range metricCh {
			ch <- m.Desc()
		}
		close(doneCh)
	}()

	e.Collect(metricCh)
	close(metricCh)
	<-doneCh
}

// Collect implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.scrape(ch)

	ch <- e.totalScrapes
	ch <- e.error
	e.scrapeErrors.Collect(ch)
	ch <- e.mysqldUp
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) {
	e.totalScrapes.Inc()
	var err error
	var wg sync.WaitGroup
	if atomic.LoadInt32(&inited) == 0 {
		mtx.Lock()
		defer mtx.Unlock()
		if atomic.LoadInt32(&inited) == 0 {
			db, err = sql.Open("mysql", e.dsn)
			if err != nil {
				log.Errorln("Error opening connection to database:", err)
				e.error.Set(1)
				return
			}
			atomic.StoreInt32(&inited, 1)
			maxCon := e.collect.MaxMySQLConns
			if maxCon > 16 {
				maxCon = 16
			}
			db.SetMaxOpenConns(maxCon)
			db.SetMaxIdleConns(1)
			db.SetConnMaxLifetime(2 * time.Minute)
		}
	}

	isUpRows, err := db.Query(upQuery)
	if err != nil {
		log.Errorln("Error pinging mysqld:", err)
		e.mysqldUp.Set(0)
		e.error.Set(1)
		return
	}

	isUpRows.Close()
	e.mysqldUp.Set(1)

	scrapeTime := time.Now()

	if e.collect.SlowLogFilter {
		wg.Add(1)
		go func() {
			defer func() {
				ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "connection")
			}()
			defer wg.Done()
			sessionSettingsRows, err := db.Query(sessionSettingsQuery)
			if err != nil {
				log.Errorln("Error setting log_slow_filter:", err)
				e.error.Set(1)
				return
			}
			sessionSettingsRows.Close()
		}()
	}

	if e.collect.GlobalStatus {
		wg.Add(1)
		go func() {
			wg.Done()
			scrapeTime = time.Now()
			if err = ScrapeGlobalStatus(db, ch); err != nil {
				log.Errorln("Error scraping for collect.global_status:", err)
				e.scrapeErrors.WithLabelValues("collect.global_status").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.global_status")
		}()
	}
	if e.collect.GlobalVariables {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeGlobalVariables(db, ch); err != nil {
				log.Errorln("Error scraping for collect.global_variables:", err)
				e.scrapeErrors.WithLabelValues("collect.global_variables").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.global_variables")
			wg.Done()
		}()
	}
	if e.collect.SlaveStatus {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeSlaveStatus(db, ch); err != nil {
				log.Errorln("Error scraping for collect.slave_status:", err)
				e.scrapeErrors.WithLabelValues("collect.slave_status").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.slave_status")
			wg.Done()
		}()
	}
	if e.collect.Processlist {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeProcesslist(db, ch); err != nil {
				log.Errorln("Error scraping for collect.info_schema.processlist:", err)
				e.scrapeErrors.WithLabelValues("collect.info_schema.processlist").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.info_schema.processlist")
			wg.Done()
		}()
	}
	if e.collect.TableSchema {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeTableSchema(db, ch, &wg); err != nil {
				log.Errorln("Error scraping for collect.info_schema.tables:", err)
				e.scrapeErrors.WithLabelValues("collect.info_schema.tables").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.info_schema.tables")
			wg.Done()
		}()
	}
	if e.collect.InnodbTablespaces {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeInfoSchemaInnodbTablespaces(db, ch); err != nil {
				log.Errorln("Error scraping for collect.info_schema.innodb_sys_tablespaces:", err)
				e.scrapeErrors.WithLabelValues("collect.info_schema.innodb_sys_tablespaces").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.info_schema.innodb_sys_tablespaces")
			wg.Done()
		}()
	}
	if e.collect.InnodbMetrics {
		wg.Add(1)
		go func() {
			if err = ScrapeInnodbMetrics(db, ch); err != nil {
				log.Errorln("Error scraping for collect.info_schema.innodb_metrics:", err)
				e.scrapeErrors.WithLabelValues("collect.info_schema.innodb_metrics").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.info_schema.innodb_metrics")
			wg.Done()
		}()
	}
	if e.collect.AutoIncrementColumns {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeAutoIncrementColumns(db, ch); err != nil {
				log.Errorln("Error scraping for collect.auto_increment.columns:", err)
				e.scrapeErrors.WithLabelValues("collect.auto_increment.columns").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.auto_increment.columns")
			wg.Done()
		}()
	}
	if e.collect.BinlogSize {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeBinlogSize(db, ch); err != nil {
				log.Errorln("Error scraping for collect.binlog_size:", err)
				e.scrapeErrors.WithLabelValues("collect.binlog_size").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.binlog_size")
			wg.Done()
		}()
	}
	if e.collect.PerfTableIOWaits {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapePerfTableIOWaits(db, ch); err != nil {
				log.Errorln("Error scraping for collect.perf_schema.tableiowaits:", err)
				e.scrapeErrors.WithLabelValues("collect.perf_schema.tableiowaits").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.perf_schema.tableiowaits")
			wg.Done()
		}()
	}
	if e.collect.PerfIndexIOWaits {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapePerfIndexIOWaits(db, ch); err != nil {
				log.Errorln("Error scraping for collect.perf_schema.indexiowaits:", err)
				e.scrapeErrors.WithLabelValues("collect.perf_schema.indexiowaits").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.perf_schema.indexiowaits")
			wg.Done()
		}()
	}
	if e.collect.PerfTableLockWaits {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapePerfTableLockWaits(db, ch); err != nil {
				log.Errorln("Error scraping for collect.perf_schema.tablelocks:", err)
				e.scrapeErrors.WithLabelValues("collect.perf_schema.tablelocks").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.perf_schema.tablelocks")
			wg.Done()
		}()
	}
	if e.collect.PerfEventsStatements {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapePerfEventsStatements(db, ch); err != nil {
				log.Errorln("Error scraping for collect.perf_schema.eventsstatements:", err)
				e.scrapeErrors.WithLabelValues("collect.perf_schema.eventsstatements").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.perf_schema.eventsstatements")
			wg.Done()
		}()
	}
	if e.collect.PerfEventsWaits {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapePerfEventsWaits(db, ch); err != nil {
				log.Errorln("Error scraping for collect.perf_schema.eventswaits:", err)
				e.scrapeErrors.WithLabelValues("collect.perf_schema.eventswaits").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.perf_schema.eventswaits")
			wg.Done()
		}()
	}
	if e.collect.PerfFileEvents {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapePerfFileEvents(db, ch); err != nil {
				log.Errorln("Error scraping for collect.perf_schema.file_events:", err)
				e.scrapeErrors.WithLabelValues("collect.perf_schema.file_events").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.perf_schema.file_events")
			wg.Done()
		}()
	}
	if e.collect.PerfFileInstances {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapePerfFileInstances(db, ch); err != nil {
				log.Errorln("Error scraping for collect.perf_schema.file_instances:", err)
				e.scrapeErrors.WithLabelValues("collect.perf_schema.file_instances").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.perf_schema.file_instances")
			wg.Done()
		}()
	}
	if e.collect.UserStat {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeUserStat(db, ch); err != nil {
				log.Errorln("Error scraping for collect.info_schema.userstats:", err)
				e.scrapeErrors.WithLabelValues("collect.info_schema.userstats").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.info_schema.userstats")
			wg.Done()
		}()
	}
	if e.collect.ClientStat {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeClientStat(db, ch); err != nil {
				log.Errorln("Error scraping for collect.info_schema.clientstats:", err)
				e.scrapeErrors.WithLabelValues("collect.info_schema.clientstats").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.info_schema.clientstats")
			wg.Done()
		}()
	}
	if e.collect.TableStat {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeTableStat(db, ch); err != nil {
				log.Errorln("Error scraping for collect.info_schema.tablestats:", err)
				e.scrapeErrors.WithLabelValues("collect.info_schema.tablestats").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.info_schema.tablestats")
			wg.Done()
		}()
	}
	if e.collect.QueryResponseTime {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeQueryResponseTime(db, ch); err != nil {
				log.Errorln("Error scraping for collect.info_schema.query_response_time:", err)
				e.scrapeErrors.WithLabelValues("collect.info_schema.query_response_time").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.info_schema.query_response_time")
			wg.Done()
		}()
	}
	if e.collect.EngineTokudbStatus {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeEngineTokudbStatus(db, ch); err != nil {
				log.Errorln("Error scraping for collect.engine_tokudb_status:", err)
				e.scrapeErrors.WithLabelValues("collect.engine_tokudb_status").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.engine_tokudb_status")
			wg.Done()
		}()
	}
	if e.collect.EngineInnodbStatus {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeEngineInnodbStatus(db, ch); err != nil {
				log.Errorln("Error scraping for collect.engine_innodb_status:", err)
				e.scrapeErrors.WithLabelValues("collect.engine_innodb_status").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.engine_innodb_status")
			wg.Done()
		}()
	}
	if e.collect.Heartbeat {
		wg.Add(1)
		go func() {
			scrapeTime = time.Now()
			if err = ScrapeHeartbeat(db, ch, e.collect.HeartbeatDatabase, e.collect.HeartbeatTable); err != nil {
				log.Errorln("Error scraping for collect.heartbeat:", err)
				e.scrapeErrors.WithLabelValues("collect.heartbeat").Inc()
				e.error.Set(1)
			}
			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(scrapeTime).Seconds(), "collect.heartbeat")
			wg.Done()
		}()
	}
	wg.Wait()
}
