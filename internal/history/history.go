// Package history keeps node, pod and container CPU/memory usage over time in a local SQLite
// file. Nothing leaves the machine.
//
// Readings are taken once a minute and kept in tiers that get coarser with age:
//
//	0–6 hours   every 1-minute reading
//	6 h–2 days  5-minute buckets: median, min, max (exact, from the 1-minute readings)
//	2–N days    15-minute buckets: median, min, max (exact, from the 1-minute readings)
//
// Each bucket records how many minutes it covers, so gaps (k8s-buddy not running) are skipped,
// not counted as zero. Rows are packed blocks (see blocks.go): one series per hour or per day.
package history

import (
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite: no cgo, so every platform builds the same way
)

const (
	rawFor     = 6 * time.Hour   // 1-minute readings kept this long
	fiveFor    = 48 * time.Hour  // 5-minute buckets kept this long
	flushEvery = 5 * time.Minute // the current hour's readings are written at least this often
	day        = 24 * time.Hour
)

// Series identifies one thing we sample: a node, a pod or a container (with its owning workload).
type Series struct {
	Cluster, Kind, Namespace, Name string
	Owner                          string // pods: "Deployment/web"; containers: "Deployment/web/app"; nodes: node pool
	CPUReq, CPULim, MemReq, MemLim int64  // pods: summed container values; nodes: allocatable in CPULim/MemLim
}

// Sample is one observation: CPU in millicores, memory in bytes.
type Sample struct {
	CPU, Mem int64
}

// Point is one bucket in a query result. CPU in millicores, memory in bytes.
type Point struct {
	T      int64   `json:"t"`    // bucket start, unix seconds
	Step   int64   `json:"step"` // bucket width in seconds: 60, 300 or 900
	N      int64   `json:"n"`    // minutes of readings in it
	CPU    float64 `json:"cpu"`  // median
	CPUMin float64 `json:"cpuMin"`
	CPUMax float64 `json:"cpuMax"`
	Mem    float64 `json:"mem"` // median
	MemMin float64 `json:"memMin"`
	MemMax float64 `json:"memMax"`
}

type Store struct {
	db        *sql.DB
	mu        sync.Mutex
	ids       map[string]int64 // series key → id
	meta      map[int64]metaSeen
	buf       map[int64]*hourBuf // the current hour's 1-minute readings per series
	lastFlush time.Time
}

type hourBuf struct {
	start int64
	b     block
	dirty bool
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

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=journal_size_limit(4194304)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; reads are quick
	// A new file gets 16 KB pages (blocks pack better) and incremental vacuum (pruned space goes
	// back to the file system). Both need the rollback journal while they're set.
	var tables, pageSize int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema`).Scan(&tables)
	_ = db.QueryRow(`PRAGMA page_size`).Scan(&pageSize)
	if tables == 0 && pageSize != 16384 {
		for _, q := range []string{`PRAGMA journal_mode = DELETE`, `PRAGMA page_size = 16384`, `PRAGMA auto_vacuum = INCREMENTAL`, `VACUUM`, `PRAGMA journal_mode = WAL`} {
			if _, err := db.Exec(q); err != nil {
				db.Close()
				return nil, fmt.Errorf("usage history: %s: %w", q, err)
			}
		}
	}
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
		CREATE TABLE IF NOT EXISTS usage (
			series INTEGER NOT NULL, tier INTEGER NOT NULL, start INTEGER NOT NULL, data BLOB NOT NULL,
			UNIQUE (series, tier, start));
		CREATE INDEX IF NOT EXISTS usage_age ON usage (tier, start);
		CREATE TABLE IF NOT EXISTS ooms (
			cluster TEXT NOT NULL, ns TEXT NOT NULL, owner TEXT NOT NULL, container TEXT NOT NULL,
			pod TEXT NOT NULL, ts INTEGER NOT NULL,
			PRIMARY KEY (cluster, ns, pod, container, ts)) WITHOUT ROWID;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("usage history: %w", err)
	}
	return &Store{db: db, ids: map[string]int64{}, meta: map[int64]metaSeen{}, buf: map[int64]*hourBuf{}, lastFlush: time.Now()}, nil
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

// Add records a batch of observations taken at `at` (one per series per minute; a second one in
// the same minute replaces the first).
func (s *Store) Add(at time.Time, obs map[Series]Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	minute := at.Truncate(time.Minute)
	hour := minute.Truncate(time.Hour).Unix()
	slot := int((minute.Unix() - hour) / 60)
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
		hb := s.buf[id]
		if hb == nil || hb.start != hour {
			if hb != nil && hb.dirty {
				if err := writeBlock(tx, id, tier1, hb.start, hb.b); err != nil {
					return err
				}
			}
			// Restarted mid-hour: carry on with what's already stored for it.
			b, err := readBlock(tx, id, tier1, hour)
			if err != nil {
				return err
			}
			hb = &hourBuf{start: hour, b: b}
			s.buf[id] = hb
		}
		kib := (v.Mem + 512) / 1024
		hb.b[slot] = bucket{N: 1, CPU: v.CPU, CPUMin: v.CPU, CPUMax: v.CPU, Mem: kib, MemMin: kib, MemMax: kib}
		hb.dirty = true
	}
	if at.Sub(s.lastFlush) >= flushEvery {
		if err := s.flushTx(tx, hour); err != nil {
			return err
		}
		s.lastFlush = at
	}
	return tx.Commit()
}

func readBlock(q execer, series int64, tier int, start int64) (block, error) {
	var data []byte
	err := q.QueryRow(`SELECT data FROM usage WHERE series = ? AND tier = ? AND start = ?`, series, tier, start).Scan(&data)
	if err == sql.ErrNoRows {
		return block{}, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := decodeBlock(tier, data)
	if err != nil {
		return block{}, nil // unreadable: start it over rather than fail recording
	}
	return b, nil
}

func writeBlock(q execer, series int64, tier int, start int64, b block) error {
	if len(b) == 0 {
		_, err := q.Exec(`DELETE FROM usage WHERE series = ? AND tier = ? AND start = ?`, series, tier, start)
		return err
	}
	_, err := q.Exec(`INSERT INTO usage (series, tier, start, data) VALUES (?, ?, ?, ?)
		ON CONFLICT (series, tier, start) DO UPDATE SET data = excluded.data`, series, tier, start, encodeBlock(tier, b))
	return err
}

// flushTx writes every changed hour buffer and drops buffers of past hours.
func (s *Store) flushTx(tx execer, hour int64) error {
	for id, hb := range s.buf {
		if hb.dirty {
			if err := writeBlock(tx, id, tier1, hb.start, hb.b); err != nil {
				return err
			}
			hb.dirty = false
		}
		if hb.start < hour {
			delete(s.buf, id)
		}
	}
	return nil
}

// Flush writes the readings gathered so far (on shutdown, and before queries).
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := s.flushTx(tx, time.Now().Truncate(time.Hour).Unix()); err != nil {
		return err
	}
	s.lastFlush = time.Now()
	return tx.Commit()
}

// Prune rolls readings into coarser tiers as they age and deletes what's past the retention:
// 1-minute hours older than 6 hours become 5-minute buckets plus exact 15-minute buckets, 5-minute
// buckets go after 2 days, and everything goes after the retention. Run it about hourly.
func (s *Store) Prune(retention time.Duration) error {
	return s.prune(time.Now(), retention)
}

func (s *Store) prune(now time.Time, retention time.Duration) error {
	if err := s.Flush(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutRaw := now.Add(-rawFor).Unix()
	cutFive := now.Add(-fiveFor).Unix()
	cutAll := now.Add(-retention).Unix()

	// 1. Roll 1-minute hours that are fully older than 6 hours.
	type rawRow struct {
		series, start int64
		data          []byte
	}
	var raws []rawRow
	rows, err := s.db.Query(`SELECT series, start, data FROM usage WHERE tier = ? AND start + 3600 <= ? ORDER BY series, start`, tier1, cutRaw)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.series, &r.start, &r.data); err != nil {
			rows.Close()
			return err
		}
		raws = append(raws, r)
	}
	rows.Close()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()          //nolint:errcheck
	days := map[[2]int64]block{} // (series, day start) → 15-minute block being filled
	for _, r := range raws {
		raw, err := decodeBlock(tier1, r.data)
		if err == nil {
			five, quarter := block{}, map[int][]bucket{}
			fiveRaw := map[int][]bucket{}
			for slot, b := range raw {
				fiveRaw[slot/5] = append(fiveRaw[slot/5], b)
				quarter[slot/15] = append(quarter[slot/15], b)
			}
			for j, bs := range fiveRaw {
				five[j] = rollRaw(bs)
			}
			if err := writeBlock(tx, r.series, tier5, r.start, five); err != nil {
				return err
			}
			dayStart := time.Unix(r.start, 0).UTC().Truncate(day).Unix()
			k := [2]int64{r.series, dayStart}
			db, ok := days[k]
			if !ok {
				if db, err = readBlock(tx, r.series, tier15, dayStart); err != nil {
					return err
				}
				days[k] = db
			}
			for q, bs := range quarter {
				db[int((r.start-dayStart)/900)+q] = rollRaw(bs) // exact: from the 1-minute readings
			}
		}
		if _, err := tx.Exec(`DELETE FROM usage WHERE series = ? AND tier = ? AND start = ?`, r.series, tier1, r.start); err != nil {
			return err
		}
	}
	for k, b := range days {
		if err := writeBlock(tx, k[0], tier15, k[1], b); err != nil {
			return err
		}
	}

	// 2. Drop 5-minute hours older than 2 days, and anything past the retention.
	if _, err := tx.Exec(`DELETE FROM usage WHERE tier = ? AND start + 3600 <= ?`, tier5, max(cutFive, cutAll)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM usage WHERE tier = ? AND start + 3600 <= ?`, tier1, cutAll); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM usage WHERE tier = ? AND start + 86400 <= ?`, tier15, cutAll); err != nil {
		return err
	}
	// The 15-minute day that straddles the retention: trim its older slots.
	var edge []rawRow
	erows, err := tx.Query(`SELECT series, start, data FROM usage WHERE tier = ? AND start < ? AND start + 86400 > ?`, tier15, cutAll, cutAll)
	if err != nil {
		return err
	}
	for erows.Next() {
		var r rawRow
		if err := erows.Scan(&r.series, &r.start, &r.data); err != nil {
			erows.Close()
			return err
		}
		edge = append(edge, r)
	}
	erows.Close()
	for _, r := range edge {
		b, err := decodeBlock(tier15, r.data)
		if err != nil {
			continue
		}
		for slot := range b {
			if r.start+int64(slot+1)*900 <= cutAll {
				delete(b, slot)
			}
		}
		if err := writeBlock(tx, r.series, tier15, r.start, b); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM ooms WHERE ts < ?`, cutAll); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM series WHERE last_seen < ? AND id NOT IN (SELECT DISTINCT series FROM usage)`, cutAll); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.ids = map[string]int64{} // ids may have been deleted; re-resolve lazily
	s.meta = map[int64]metaSeen{}
	_, err = s.db.Exec(`PRAGMA incremental_vacuum`) // give freed pages back to the file system
	return err
}

// ---- reads ----

type tierPoint struct {
	t, step int64
	b       bucket
}

// readPoints returns each series' points since `since`, finest tier first: 1-minute readings, then
// 5-minute buckets before them, then 15-minute buckets before those. `where` filters series rows
// (aliased se); its args follow `since`.
func (s *Store) readPoints(since int64, where string, args ...any) (map[int64][]tierPoint, map[int64][2]string, error) {
	if err := s.Flush(); err != nil {
		return nil, nil, err
	}
	rows, err := s.db.Query(`SELECT u.series, u.tier, u.start, u.data, se.ns, se.owner FROM usage u JOIN series se ON se.id = u.series
		WHERE u.start >= ? AND `+where, append([]any{since - 86400}, args...)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byTier := map[int64]map[int][]tierPoint{}
	names := map[int64][2]string{}
	for rows.Next() {
		var id, start int64
		var tier int
		var data []byte
		var ns, owner string
		if err := rows.Scan(&id, &tier, &start, &data, &ns, &owner); err != nil {
			return nil, nil, err
		}
		b, err := decodeBlock(tier, data)
		if err != nil {
			continue
		}
		names[id] = [2]string{ns, owner}
		if byTier[id] == nil {
			byTier[id] = map[int][]tierPoint{}
		}
		step := int64(tier * 60)
		for slot, bk := range b {
			byTier[id][tier] = append(byTier[id][tier], tierPoint{start + int64(slot)*step, step, bk})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	out := make(map[int64][]tierPoint, len(byTier))
	for id, tiers := range byTier {
		lo1, lo5 := int64(math.MaxInt64), int64(math.MaxInt64)
		for _, p := range tiers[tier1] {
			lo1 = min(lo1, p.t)
		}
		lo5 = lo1
		var pts []tierPoint
		for _, p := range tiers[tier1] {
			if p.t >= since {
				pts = append(pts, p)
			}
		}
		for _, p := range tiers[tier5] {
			if p.t < lo1 {
				lo5 = min(lo5, p.t)
				if p.t+p.step > since {
					pts = append(pts, p)
				}
			}
		}
		for _, p := range tiers[tier15] {
			if p.t+p.step <= lo5 && p.t+p.step > since {
				pts = append(pts, p)
			}
		}
		slices.SortFunc(pts, func(a, b tierPoint) int { return int(a.t - b.t) })
		out[id] = pts
	}
	return out, names, nil
}

func toPoint(p tierPoint) Point {
	return Point{T: p.t, Step: p.step, N: p.b.N, CPU: float64(p.b.CPU), CPUMin: float64(p.b.CPUMin), CPUMax: float64(p.b.CPUMax),
		Mem: float64(p.b.Mem * 1024), MemMin: float64(p.b.MemMin * 1024), MemMax: float64(p.b.MemMax * 1024)}
}

// SeriesInfo is the latest requests/limits recorded for a series.
type SeriesInfo struct {
	CPUReq int64 `json:"cpuReq"`
	CPULim int64 `json:"cpuLim"`
	MemReq int64 `json:"memReq"`
	MemLim int64 `json:"memLim"`
	Pods   int   `json:"pods,omitempty"` // workloads: how many pods contributed
}

// Query returns points since `since` for a node, a pod, or a workload (kind "workload" with
// name "Kind/name": the sum over its pods per point).
func (s *Store) Query(cluster, kind, ns, name string, since time.Time) ([]Point, SeriesInfo, error) {
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
	byID, _, err := s.readPoints(since.Unix(), strings.ReplaceAll("se."+where, " AND ", " AND se."), args...)
	if err != nil {
		return nil, info, err
	}
	// Requests/limits: the most recently seen series (summed for a workload's current pods).
	row := s.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN cpu_req >= 0 THEN cpu_req END), -1), COALESCE(SUM(CASE WHEN cpu_lim >= 0 THEN cpu_lim END), -1),
		COALESCE(SUM(CASE WHEN mem_req >= 0 THEN mem_req END), -1), COALESCE(SUM(CASE WHEN mem_lim >= 0 THEN mem_lim END), -1), COUNT(*)
		FROM series WHERE `+where+` AND last_seen >= (SELECT MAX(last_seen) FROM series WHERE `+where+`) - 600`, append(args, args...)...)
	if err := row.Scan(&info.CPUReq, &info.CPULim, &info.MemReq, &info.MemLim, &info.Pods); err != nil && err != sql.ErrNoRows {
		return nil, info, err
	}
	sum := map[int64]*Point{}
	for _, pts := range byID {
		for _, tp := range pts {
			p := toPoint(tp)
			if a := sum[p.T]; a != nil {
				a.CPU, a.CPUMin, a.CPUMax = a.CPU+p.CPU, a.CPUMin+p.CPUMin, a.CPUMax+p.CPUMax
				a.Mem, a.MemMin, a.MemMax = a.Mem+p.Mem, a.MemMin+p.MemMin, a.MemMax+p.MemMax
				a.Step, a.N = max(a.Step, p.Step), max(a.N, p.N)
			} else {
				sum[p.T] = &p
			}
		}
	}
	pts := make([]Point, 0, len(sum))
	for _, p := range sum {
		pts = append(pts, *p)
	}
	slices.SortFunc(pts, func(a, b Point) int { return int(a.T - b.T) })
	return pts, info, nil
}

// Stats summarises what's stored, for Settings: series, stored blocks, oldest data.
func (s *Store) Stats() (series, samples int64, oldest time.Time, err error) {
	var o sql.NullInt64
	err = s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM series), (SELECT COUNT(*) FROM usage), (SELECT MIN(start) FROM usage)`).Scan(&series, &samples, &o)
	if o.Valid {
		oldest = time.Unix(o.Int64, 0)
	}
	return
}

// Size is the database's size on disk in bytes.
func (s *Store) Size() int64 {
	var pages, size int64
	_ = s.db.QueryRow(`SELECT page_count, page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&pages, &size)
	return pages * size
}

// weighted percentile: values with weights (minutes covered).
func wpct(v, w []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	idx := make([]int, len(v))
	var total float64
	for i := range idx {
		idx[i] = i
		total += w[i]
	}
	slices.SortFunc(idx, func(a, b int) int { return cmpF(v[a], v[b]) })
	target, acc := p*total, 0.0
	for _, i := range idx {
		acc += w[i]
		if acc >= target {
			return v[i]
		}
	}
	return v[idx[len(idx)-1]]
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Peak summarises one pod's recorded usage over a window: the 95th percentile and the maximum of
// its peaks.
type Peak struct {
	CPU95, Mem95   float64
	CPUMax, MemMax float64
	Buckets        int   // points with data
	Minutes        int64 // minutes of readings behind them
}

// PodPeaks returns usage peaks per pod ("namespace/name") of a cluster since `since`, and how many
// hours of history the cluster has in that window.
func (s *Store) PodPeaks(cluster string, since time.Time) (map[string]Peak, float64, error) {
	byID, _, err := s.readPoints(since.Unix(), `se.cluster = ? AND se.kind = 'pod'`, cluster)
	if err != nil {
		return nil, 0, err
	}
	names := map[int64]string{}
	rows, err := s.db.Query(`SELECT id, ns, name FROM series WHERE cluster = ? AND kind = 'pod'`, cluster)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var id int64
		var ns, name string
		if rows.Scan(&id, &ns, &name) == nil {
			names[id] = ns + "/" + name
		}
	}
	rows.Close()
	out := map[string]Peak{}
	lo, hi := int64(math.MaxInt64), int64(0)
	for id, pts := range byID {
		var cpu, mem, w []float64
		for _, p := range pts {
			cpu, mem, w = append(cpu, float64(p.b.CPUMax)), append(mem, float64(p.b.MemMax*1024)), append(w, float64(p.b.N))
			lo, hi = min(lo, p.t), max(hi, p.t+p.step)
		}
		if len(cpu) > 0 {
			var mins int64
			for _, x := range w {
				mins += int64(x)
			}
			out[names[id]] = Peak{CPU95: wpct(cpu, w, .95), Mem95: wpct(mem, w, .95), CPUMax: slices.Max(cpu), MemMax: slices.Max(mem), Buckets: len(cpu), Minutes: mins}
		}
	}
	hours := 0.0
	if hi > 0 {
		hours = float64(hi-lo) / 3600
	}
	return out, hours, nil
}

// Profile summarises one workload's usage per replica over a window, from the time that has data
// only: time k8s-buddy wasn't recording is skipped, not counted as zero.
type Profile struct {
	Namespace, Owner string
	Minutes          int64 // minutes with readings
	First, Last      int64 // unix seconds: start of the first point, end of the last
	// Per replica, in millicores and bytes. Median and p95 of the typical (median) use, weighted by
	// minutes; Max is the highest single reading of any replica.
	CPUMed, CPUP95, CPUMax float64
	MemMed, MemP95, MemMax float64
}

// Profiles returns per-workload usage profiles of a cluster since `since`. kind is "pod" (owner
// "Kind/name") or "container" (owner "Kind/name/container").
func (s *Store) Profiles(cluster, kind string, since time.Time) ([]Profile, error) {
	byID, names, err := s.readPoints(since.Unix(), `se.cluster = ? AND se.kind = ? AND se.owner != ''`, cluster, kind)
	if err != nil {
		return nil, err
	}
	type agg struct {
		n        int
		cpu, mem float64 // sums of medians across replicas
		cmax     float64
		mmax     float64
		minutes  int64
		step     int64
	}
	byOwner := map[[2]string]map[int64]*agg{}
	for id, pts := range byID {
		k := names[id]
		if byOwner[k] == nil {
			byOwner[k] = map[int64]*agg{}
		}
		for _, p := range pts {
			a := byOwner[k][p.t]
			if a == nil {
				a = &agg{}
				byOwner[k][p.t] = a
			}
			a.n++
			a.cpu += float64(p.b.CPU)
			a.mem += float64(p.b.Mem * 1024)
			a.cmax = max(a.cmax, float64(p.b.CPUMax))
			a.mmax = max(a.mmax, float64(p.b.MemMax*1024))
			a.minutes = max(a.minutes, p.b.N)
			a.step = max(a.step, p.step)
		}
	}
	out := make([]Profile, 0, len(byOwner))
	for k, ts := range byOwner {
		p := Profile{Namespace: k[0], Owner: k[1], First: math.MaxInt64}
		var cpu, mem, w []float64
		for t, a := range ts {
			cpu, mem, w = append(cpu, a.cpu/float64(a.n)), append(mem, a.mem/float64(a.n)), append(w, float64(a.minutes))
			p.Minutes += a.minutes
			p.CPUMax, p.MemMax = max(p.CPUMax, a.cmax), max(p.MemMax, a.mmax)
			p.First, p.Last = min(p.First, t), max(p.Last, t+a.step)
		}
		p.CPUMed, p.CPUP95 = wpct(cpu, w, .5), wpct(cpu, w, .95)
		p.MemMed, p.MemP95 = wpct(mem, w, .5), wpct(mem, w, .95)
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Profile) int { return strings.Compare(a.Namespace+"/"+a.Owner, b.Namespace+"/"+b.Owner) })
	return out, nil
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
	Count  int
	Last   time.Time
	MaxDay int // most kills within any 24 hours: repeated kills keep it down in restart back-off
}

// OOMs returns OOM kills per "ns/Kind/name/container" of a cluster since `since`.
func (s *Store) OOMs(cluster string, since time.Time) (map[string]OOMStat, error) {
	rows, err := s.db.Query(`SELECT ns, owner, container, ts FROM ooms WHERE cluster = ? AND ts >= ? ORDER BY ns, owner, container, ts`, cluster, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	times := map[string][]int64{}
	for rows.Next() {
		var ns, owner, ct string
		var ts int64
		if err := rows.Scan(&ns, &owner, &ct, &ts); err != nil {
			return nil, err
		}
		k := ns + "/" + owner + "/" + ct
		times[k] = append(times[k], ts)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]OOMStat, len(times))
	for k, ts := range times { // sorted by time
		st := OOMStat{Count: len(ts), Last: time.Unix(ts[len(ts)-1], 0)}
		for i, j := 0, 0; j < len(ts); j++ {
			for ts[j]-ts[i] >= 86400 {
				i++
			}
			st.MaxDay = max(st.MaxDay, j-i+1)
		}
		out[k] = st
	}
	return out, nil
}
