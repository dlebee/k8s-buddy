// Package history keeps node and pod CPU/memory usage over time in a local SQLite file, as
// 5-minute buckets (average and peak). Nothing leaves the machine.
package history

import (
	"database/sql"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite: no cgo, so every platform builds the same way
)

const Bucket = 5 * time.Minute

// Series identifies one thing we sample: a node, or a pod (with its owning workload).
type Series struct {
	Cluster, Kind, Namespace, Name string
	Owner                          string // pods: "Deployment/web"; nodes: node pool
	CPUReq, CPULim, MemReq, MemLim int64  // pods: summed container values; nodes: allocatable in CPULim/MemLim
}

// Sample is one observation: CPU in millicores, memory in bytes.
type Sample struct {
	CPU, Mem int64
}

// Point is one bucket in a query result.
type Point struct {
	T      int64   `json:"t"` // bucket start, unix seconds
	CPU    float64 `json:"cpu"`
	CPUMax float64 `json:"cpuMax"`
	Mem    float64 `json:"mem"`
	MemMax float64 `json:"memMax"`
}

type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	ids  map[string]int64 // series key → id
	meta map[int64]metaSeen
	acc  map[int64]*accum // samples of the current bucket
	cur  int64            // current bucket start
}

// metaSeen is what we last wrote for a series, so unchanged pods aren't rewritten every minute.
type metaSeen struct {
	owner          string
	cpuReq, cpuLim int64
	memReq, memLim int64
	at             int64
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

type accum struct {
	n              int
	cpuSum, memSum int64
	cpuMax, memMax int64
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; reads are quick
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS series (
			id INTEGER PRIMARY KEY,
			cluster TEXT NOT NULL, kind TEXT NOT NULL, ns TEXT NOT NULL, name TEXT NOT NULL,
			owner TEXT NOT NULL DEFAULT '',
			cpu_req INTEGER NOT NULL DEFAULT -1, cpu_lim INTEGER NOT NULL DEFAULT -1,
			mem_req INTEGER NOT NULL DEFAULT -1, mem_lim INTEGER NOT NULL DEFAULT -1,
			last_seen INTEGER NOT NULL DEFAULT 0,
			UNIQUE (cluster, kind, ns, name));
		CREATE INDEX IF NOT EXISTS series_owner ON series (cluster, kind, ns, owner);
		CREATE TABLE IF NOT EXISTS samples (
			series INTEGER NOT NULL, ts INTEGER NOT NULL,
			cpu_avg REAL NOT NULL, cpu_max REAL NOT NULL, mem_avg REAL NOT NULL, mem_max REAL NOT NULL,
			PRIMARY KEY (series, ts)) WITHOUT ROWID;
		CREATE INDEX IF NOT EXISTS samples_ts ON samples (ts);
		CREATE TABLE IF NOT EXISTS ooms (
			cluster TEXT NOT NULL, ns TEXT NOT NULL, owner TEXT NOT NULL, container TEXT NOT NULL,
			pod TEXT NOT NULL, ts INTEGER NOT NULL,
			PRIMARY KEY (cluster, ns, pod, container, ts)) WITHOUT ROWID;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("usage history: %w", err)
	}
	return &Store{db: db, ids: map[string]int64{}, meta: map[int64]metaSeen{}, acc: map[int64]*accum{}}, nil
}

func (s *Store) Close() error {
	s.Flush()
	return s.db.Close()
}

func key(c, k, ns, n string) string { return c + "\x00" + k + "\x00" + ns + "\x00" + n }

func (s *Store) seriesID(q execer, sr Series, now int64) (int64, error) {
	k := key(sr.Cluster, sr.Kind, sr.Namespace, sr.Name)
	id, ok := s.ids[k]
	if !ok {
		err := q.QueryRow(`INSERT INTO series (cluster, kind, ns, name) VALUES (?, ?, ?, ?)
			ON CONFLICT (cluster, kind, ns, name) DO UPDATE SET name = excluded.name RETURNING id`,
			sr.Cluster, sr.Kind, sr.Namespace, sr.Name).Scan(&id)
		if err != nil {
			return 0, err
		}
		s.ids[k] = id
	}
	m := metaSeen{sr.Owner, sr.CPUReq, sr.CPULim, sr.MemReq, sr.MemLim, now}
	if prev, ok := s.meta[id]; ok && prev.owner == m.owner && prev.cpuReq == m.cpuReq && prev.cpuLim == m.cpuLim &&
		prev.memReq == m.memReq && prev.memLim == m.memLim && now-prev.at < 300 {
		return id, nil
	}
	_, err := q.Exec(`UPDATE series SET owner = ?, cpu_req = ?, cpu_lim = ?, mem_req = ?, mem_lim = ?, last_seen = ? WHERE id = ?`,
		sr.Owner, sr.CPUReq, sr.CPULim, sr.MemReq, sr.MemLim, now, id)
	if err == nil {
		s.meta[id] = m
	}
	return id, err
}

// Add records a batch of observations taken at `at`. When `at` falls in a new bucket the previous
// one is written to disk first.
func (s *Store) Add(at time.Time, obs map[Series]Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := at.Truncate(Bucket).Unix()
	if s.cur != 0 && b != s.cur {
		if err := s.flushLocked(); err != nil {
			return err
		}
		s.resetBucket()
	}
	s.cur = b
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below on success
	for sr, v := range obs {
		id, err := s.seriesID(tx, sr, at.Unix())
		if err != nil {
			return err
		}
		a := s.acc[id]
		if a == nil {
			a = &accum{}
			s.acc[id] = a
		}
		a.n++
		a.cpuSum += v.CPU
		a.memSum += v.Mem
		a.cpuMax = max(a.cpuMax, v.CPU)
		a.memMax = max(a.memMax, v.Mem)
	}
	return tx.Commit()
}

// Flush writes the current partial bucket (e.g. on shutdown, or so queries include it).
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	if len(s.acc) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO samples (series, ts, cpu_avg, cpu_max, mem_avg, mem_max) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (series, ts) DO UPDATE SET cpu_avg = excluded.cpu_avg, cpu_max = max(cpu_max, excluded.cpu_max),
		mem_avg = excluded.mem_avg, mem_max = max(mem_max, excluded.mem_max)`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	for id, a := range s.acc {
		if _, err := stmt.Exec(id, s.cur, float64(a.cpuSum)/float64(a.n), float64(a.cpuMax), float64(a.memSum)/float64(a.n), float64(a.memMax)); err != nil {
			tx.Rollback() //nolint:errcheck
			return err
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		return err
	}
	// Keep the running bucket's accumulators when flushing mid-bucket (queries call Flush); a
	// rollover in Add clears them because the next bucket starts fresh.
	return nil
}

// resetBucket starts an empty bucket after a rollover (flushLocked keeps accumulators so a
// mid-bucket flush for a query doesn't lose the samples gathered so far).
func (s *Store) resetBucket() { s.acc = map[int64]*accum{} }

// Prune deletes samples older than the retention, and series with nothing left.
func (s *Store) Prune(retention time.Duration) error {
	cut := time.Now().Add(-retention).Unix()
	if _, err := s.db.Exec(`DELETE FROM samples WHERE ts < ?`, cut); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM ooms WHERE ts < ?`, cut); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM series WHERE last_seen < ? AND id NOT IN (SELECT DISTINCT series FROM samples)`, cut)
	if err == nil {
		s.mu.Lock()
		s.ids = map[string]int64{} // ids may have been deleted; re-resolve lazily
		s.meta = map[int64]metaSeen{}
		s.mu.Unlock()
	}
	return err
}

// SeriesInfo is the latest requests/limits recorded for a series.
type SeriesInfo struct {
	CPUReq int64 `json:"cpuReq"`
	CPULim int64 `json:"cpuLim"`
	MemReq int64 `json:"memReq"`
	MemLim int64 `json:"memLim"`
	Pods   int   `json:"pods,omitempty"` // workloads: how many pods contributed
}

// Query returns buckets since `since` for a node, a pod, or a workload (kind "workload" with
// name "Kind/name": the sum over its pods per bucket).
func (s *Store) Query(cluster, kind, ns, name string, since time.Time) ([]Point, SeriesInfo, error) {
	s.Flush() //nolint:errcheck // include the bucket in progress
	var info SeriesInfo
	var where string
	args := []any{cluster}
	switch kind {
	case "workload":
		where = `cluster = ? AND kind = 'pod' AND ns = ? AND owner = ?`
		args = append(args, ns, name)
	default:
		where = `cluster = ? AND kind = ? AND ns = ? AND name = ?`
		args = append(args, kind, ns, name)
	}
	// Requests/limits: the most recently seen series (summed for a workload's current pods).
	row := s.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN cpu_req >= 0 THEN cpu_req END), -1), COALESCE(SUM(CASE WHEN cpu_lim >= 0 THEN cpu_lim END), -1),
		COALESCE(SUM(CASE WHEN mem_req >= 0 THEN mem_req END), -1), COALESCE(SUM(CASE WHEN mem_lim >= 0 THEN mem_lim END), -1), COUNT(*)
		FROM series WHERE `+where+` AND last_seen >= (SELECT MAX(last_seen) FROM series WHERE `+where+`) - 600`, append(args, args...)...)
	if err := row.Scan(&info.CPUReq, &info.CPULim, &info.MemReq, &info.MemLim, &info.Pods); err != nil && err != sql.ErrNoRows {
		return nil, info, err
	}
	rows, err := s.db.Query(`SELECT ts, SUM(cpu_avg), SUM(cpu_max), SUM(mem_avg), SUM(mem_max) FROM samples
		WHERE ts >= ? AND series IN (SELECT id FROM series WHERE `+where+`) GROUP BY ts ORDER BY ts`, append([]any{since.Unix()}, args...)...)
	if err != nil {
		return nil, info, err
	}
	defer rows.Close()
	pts := []Point{}
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.T, &p.CPU, &p.CPUMax, &p.Mem, &p.MemMax); err != nil {
			return nil, info, err
		}
		pts = append(pts, p)
	}
	return pts, info, rows.Err()
}

// Stats summarises what's stored, for Settings.
func (s *Store) Stats() (series, samples int64, oldest time.Time, err error) {
	var o sql.NullInt64
	err = s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM series), (SELECT COUNT(*) FROM samples), (SELECT MIN(ts) FROM samples)`).Scan(&series, &samples, &o)
	if o.Valid {
		oldest = time.Unix(o.Int64, 0)
	}
	return
}

// Size is the database's size on disk in bytes (main file plus write-ahead log).
func (s *Store) Size() int64 {
	var pages, size int64
	_ = s.db.QueryRow(`SELECT page_count, page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&pages, &size)
	return pages * size
}

// Peak summarises one pod's recorded usage over a window: the 95th percentile and the maximum of
// its 5-minute peaks.
type Peak struct {
	CPU95, Mem95   float64
	CPUMax, MemMax float64
	Buckets        int
}

// PodPeaks returns usage peaks per pod ("namespace/name") of a cluster since `since`, and how many
// hours of history the cluster has in that window.
func (s *Store) PodPeaks(cluster string, since time.Time) (map[string]Peak, float64, error) {
	s.Flush() //nolint:errcheck
	rows, err := s.db.Query(`SELECT se.ns, se.name, sa.ts, sa.cpu_max, sa.mem_max FROM samples sa JOIN series se ON se.id = sa.series
		WHERE se.cluster = ? AND se.kind = 'pod' AND sa.ts >= ? ORDER BY se.id`, cluster, since.Unix())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	type acc struct{ cpu, mem []float64 }
	by := map[string]*acc{}
	var lo, hi int64
	for rows.Next() {
		var ns, name string
		var ts int64
		var cpu, mem float64
		if err := rows.Scan(&ns, &name, &ts, &cpu, &mem); err != nil {
			return nil, 0, err
		}
		k := ns + "/" + name
		if by[k] == nil {
			by[k] = &acc{}
		}
		by[k].cpu = append(by[k].cpu, cpu)
		by[k].mem = append(by[k].mem, mem)
		if lo == 0 || ts < lo {
			lo = ts
		}
		hi = max(hi, ts)
	}
	out := make(map[string]Peak, len(by))
	for k, a := range by {
		out[k] = Peak{CPU95: pct(a.cpu, .95), Mem95: pct(a.mem, .95), CPUMax: pct(a.cpu, 1), MemMax: pct(a.mem, 1), Buckets: len(a.cpu)}
	}
	hours := 0.0
	if hi > 0 {
		hours = float64(hi-lo+int64(Bucket.Seconds())) / 3600
	}
	return out, hours, rows.Err()
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

// Profile summarises one workload's usage per replica over a window, from the buckets that have
// data only: time the app wasn't recording is skipped, not counted as zero.
type Profile struct {
	Namespace, Owner string
	Buckets          int   // 5-minute buckets with data
	First, Last      int64 // unix seconds of the first and last bucket
	// Per replica, in millicores and bytes. Median and p95 of the typical (average) usage per
	// bucket; Max is the highest single reading of any replica.
	CPUMed, CPUP95, CPUMax float64
	MemMed, MemP95, MemMax float64
}

// Profiles returns per-workload usage profiles of a cluster since `since`. kind is "pod" (owner
// "Kind/name") or "container" (owner "Kind/name/container").
func (s *Store) Profiles(cluster, kind string, since time.Time) ([]Profile, error) {
	s.Flush() //nolint:errcheck
	rows, err := s.db.Query(`SELECT se.ns, se.owner, sa.ts, AVG(sa.cpu_avg), MAX(sa.cpu_max), AVG(sa.mem_avg), MAX(sa.mem_max)
		FROM samples sa JOIN series se ON se.id = sa.series
		WHERE se.cluster = ? AND se.kind = ? AND se.owner != '' AND sa.ts >= ?
		GROUP BY se.ns, se.owner, sa.ts ORDER BY se.ns, se.owner, sa.ts`, cluster, kind, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	var cur *Profile
	var cpu, mem []float64
	done := func() {
		if cur == nil {
			return
		}
		cur.CPUMed, cur.CPUP95 = pct(cpu, .5), pct(cpu, .95)
		cur.MemMed, cur.MemP95 = pct(mem, .5), pct(mem, .95)
		out = append(out, *cur)
		cpu, mem = cpu[:0], mem[:0]
	}
	for rows.Next() {
		var ns, owner string
		var ts int64
		var ca, cm, ma, mm float64
		if err := rows.Scan(&ns, &owner, &ts, &ca, &cm, &ma, &mm); err != nil {
			return nil, err
		}
		if cur == nil || cur.Namespace != ns || cur.Owner != owner {
			done()
			cur = &Profile{Namespace: ns, Owner: owner, First: ts}
		}
		cur.Buckets++
		cur.Last = ts
		cur.CPUMax, cur.MemMax = max(cur.CPUMax, cm), max(cur.MemMax, mm)
		cpu, mem = append(cpu, ca), append(mem, ma)
	}
	done()
	return out, rows.Err()
}

// OOM is one container OOM kill: owner "Kind/name", ts when the container was terminated.
type OOM struct {
	Cluster, Namespace, Owner, Container, Pod string
	At                                        time.Time
}

// AddOOMs records OOM kills; the same kill seen again is ignored.
func (s *Store) AddOOMs(ev []OOM) error {
	for _, e := range ev {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO ooms (cluster, ns, owner, container, pod, ts) VALUES (?, ?, ?, ?, ?, ?)`,
			e.Cluster, e.Namespace, e.Owner, e.Container, e.Pod, e.At.Unix()); err != nil {
			return err
		}
	}
	return nil
}

// OOMStat is how often one workload's container was OOM-killed in a window.
type OOMStat struct {
	Count int
	Last  time.Time
}

// OOMs returns OOM kills per "ns/Kind/name/container" of a cluster since `since`.
func (s *Store) OOMs(cluster string, since time.Time) (map[string]OOMStat, error) {
	rows, err := s.db.Query(`SELECT ns, owner, container, COUNT(*), MAX(ts) FROM ooms WHERE cluster = ? AND ts >= ? GROUP BY ns, owner, container`, cluster, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]OOMStat{}
	for rows.Next() {
		var ns, owner, ct string
		var n int
		var last int64
		if err := rows.Scan(&ns, &owner, &ct, &n, &last); err != nil {
			return nil, err
		}
		out[ns+"/"+owner+"/"+ct] = OOMStat{Count: n, Last: time.Unix(last, 0)}
	}
	return out, rows.Err()
}
