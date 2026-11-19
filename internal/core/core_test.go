package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func encrypt(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewEncWriter(&buf, key)
	if err != nil {
		t.Fatal(err)
	}
	// odd write sizes on purpose
	for off := 0; off < len(plain); off += 70001 {
		end := min(off+70001, len(plain))
		if _, err := w.Write(plain[off:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decrypt(key, enc []byte) ([]byte, error) {
	r, err := NewDecReader(bytes.NewReader(enc), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestEncryptRoundTripSizes(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	for _, n := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 123} {
		plain := make([]byte, n)
		rand.Read(plain)
		got, err := decrypt(key, encrypt(t, key, plain))
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("size %d: round trip failed: %v", n, err)
		}
	}
}

func TestEncryptDetectsDamage(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	plain := make([]byte, 2*chunkSize+500)
	rand.Read(plain)
	enc := encrypt(t, key, plain)

	// cut exactly at a frame boundary: the last-frame flag must catch it
	firstFrame := 20 + 5 + chunkSize + 16
	if _, err := decrypt(key, enc[:firstFrame]); err == nil {
		t.Fatal("a file cut at a frame boundary was accepted")
	}
	if _, err := decrypt(key, enc[:len(enc)-7]); err == nil {
		t.Fatal("a file cut mid-frame was accepted")
	}
	flipped := append([]byte{}, enc...)
	flipped[len(flipped)/2] ^= 1
	if _, err := decrypt(key, flipped); err == nil {
		t.Fatal("a flipped bit was accepted")
	}
	other := make([]byte, 32)
	rand.Read(other)
	if _, err := decrypt(other, enc); err == nil {
		t.Fatal("a wrong key was accepted")
	}
	// swap frame 1 and frame 2 (same size): the counter nonce must catch it
	a, b := 20, 20+5+chunkSize+16
	swapped := append([]byte{}, enc[:a]...)
	swapped = append(swapped, enc[b:b+5+chunkSize+16]...)
	swapped = append(swapped, enc[a:b]...)
	swapped = append(swapped, enc[b+5+chunkSize+16:]...)
	if _, err := decrypt(key, swapped); err == nil {
		t.Fatal("reordered frames were accepted")
	}
}

func TestKeyringSeal(t *testing.T) {
	dir := t.TempDir()
	k, created, err := LoadOrCreateMaster(filepath.Join(dir, "k", "master.key"))
	if err != nil || !created {
		t.Fatal(err, created)
	}
	k2, created, _ := LoadOrCreateMaster(filepath.Join(dir, "k", "master.key"))
	if created {
		t.Fatal("key was recreated")
	}
	sealed, _ := k.Seal([]byte("s3cret"))
	if bytes.Contains(sealed, []byte("s3cret")) {
		t.Fatal("secret stored in clear")
	}
	if out, err := k2.Open(sealed); err != nil || string(out) != "s3cret" {
		t.Fatal("reloaded key cannot open", err)
	}
}

func TestSQLValueWriting(t *testing.T) {
	cases := []struct {
		kind int
		in   sql.RawBytes
		want string
	}{
		{kindText, sql.RawBytes("it's"), `'it\'s'`},
		{kindText, sql.RawBytes("a\\b\nc\r\x00\x1a"), `'a\\b\nc\r\0\Z'`},
		{kindText, sql.RawBytes("عربي ✓"), "'عربي ✓'"},
		{kindText, sql.RawBytes(""), "''"},
		{kindText, nil, "NULL"},
		{kindNumber, sql.RawBytes("-12.50"), "-12.50"},
		{kindBinary, sql.RawBytes{0x00, 0xff, 0x27}, "0x00ff27"},
		{kindBinary, sql.RawBytes{}, "''"},
		{kindText, sql.RawBytes("x'); DROP TABLE t; --"), `'x\'); DROP TABLE t; --'`},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		w := bufio.NewWriter(&buf)
		writeValue(w, c.kind, c.in)
		w.Flush()
		if buf.String() != c.want {
			t.Errorf("%q: got %s want %s", c.in, buf.String(), c.want)
		}
	}
	for typ, want := range map[string]int{"BIGINT": kindNumber, "UNSIGNED INT": kindNumber, "DECIMAL": kindNumber, "DOUBLE": kindNumber, "VARCHAR": kindText,
		"TEXT": kindText, "DATETIME": kindText, "JSON": kindText, "BLOB": kindBinary, "VARBINARY": kindBinary, "BIT": kindBinary, "GEOMETRY": kindBinary, "ENUM": kindText} {
		if columnKind(typ) != want {
			t.Errorf("columnKind(%s) = %d, want %d", typ, columnKind(typ), want)
		}
	}
}

func TestSpecSelection(t *testing.T) {
	all := Spec{}
	if !all.wantsTable("shop", "orders") {
		t.Error("whole server must include everything")
	}
	dbs := Spec{Databases: []string{"shop"}, Exclude: []string{"shop.logs_*"}}
	if !dbs.wantsTable("shop", "orders") || dbs.wantsTable("other", "orders") || dbs.wantsTable("shop", "logs_2026") {
		t.Error("database scope / exclude wrong")
	}
	tb := Spec{Tables: []string{"shop.orders", "shop.radius_*"}}
	if !tb.wantsTable("shop", "orders") || !tb.wantsTable("shop", "radius_logs") || tb.wantsTable("shop", "users") || tb.wantsTable("x", "orders") {
		t.Error("table scope wrong")
	}
	bad := Spec{Content: "everything"}
	if bad.Normalize() == nil {
		t.Error("bad content accepted")
	}
	if (&Spec{Tables: []string{"orders"}}).Normalize() == nil {
		t.Error("table without database accepted")
	}
}

func TestRetention(t *testing.T) {
	now := int64(1_800_000_000)
	day := int64(86400)
	mk := func(id string, ageDays int64, status string, pinned bool) *Backup {
		return &Backup{ID: id, CreatedAt: now - ageDays*day, Status: status, Pinned: pinned}
	}
	ids := func(l []*Backup) string {
		var s []string
		for _, b := range l {
			s = append(s, b.ID)
		}
		return strings.Join(s, ",")
	}
	list := []*Backup{mk("a", 1, "done", false), mk("b", 5, "done", false), mk("c", 20, "done", false), mk("d", 30, "done", true), mk("e", 40, "done", false), mk("f", 50, "failed", false)}
	if got := ids(RetentionVictims(list, 14, 0, now)); got != "c,e" {
		t.Errorf("keep 14 days: %s", got)
	}
	if got := ids(RetentionVictims(list, 0, 2, now)); got != "c,e" {
		t.Errorf("keep last 2: %s", got)
	}
	// both rules: must be old AND beyond the count
	if got := ids(RetentionVictims(list, 3, 4, now)); got != "e" {
		t.Errorf("both rules: %s", got)
	}
	if got := RetentionVictims(list, 0, 0, now); got != nil {
		t.Errorf("no rules must delete nothing: %s", ids(got))
	}
	// everything is old: the newest good one still stays
	old := []*Backup{mk("x", 100, "done", false), mk("y", 200, "done", false), mk("z", 90, "failed", false)}
	if got := ids(RetentionVictims(old, 7, 0, now)); got != "y" {
		t.Errorf("newest good backup must survive: %s", got)
	}
}

func TestLocalStorageStaysInsideItsFolder(t *testing.T) {
	root := t.TempDir()
	st := &localStore{root: filepath.Join(root, "dest")}
	ctx := context.Background()
	if _, err := st.Put(ctx, "../escape.txt", strings.NewReader("x")); err == nil {
		t.Fatal("a key outside the folder was written")
	}
	if err := st.DeletePrefix(ctx, ""); err == nil {
		t.Fatal("deleting the whole destination was allowed")
	}
	if _, err := st.Put(ctx, "b1/db/t.sql", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	rc, n, err := st.Open(ctx, "b1/db/t.sql")
	if err != nil || n != 5 {
		t.Fatal(err, n)
	}
	rc.Close()
	// a failed upload leaves no file behind
	if _, err := st.Put(ctx, "b1/db/bad.sql", io.MultiReader(strings.NewReader("part"), errReader{})); err == nil {
		t.Fatal("expected the upload to fail")
	}
	if _, _, err := st.Open(ctx, "b1/db/bad.sql"); err == nil {
		t.Fatal("a partial file was left behind")
	}
	if err := st.DeletePrefix(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Open(ctx, "b1/db/t.sql"); err == nil {
		t.Fatal("file still there after delete")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("network dropped") }

func newTestQueue(t *testing.T) (*Queue, *Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	q := NewQueue(st, NewHub(), 2)
	q.RetryBase = 50 * time.Millisecond
	return q, st, path
}

func waitState(t *testing.T, q *Queue, id int64, want string) *Job {
	t.Helper()
	for i := 0; i < 400; i++ {
		if j, _ := q.Job(id); j != nil && j.State == want {
			return j
		}
		time.Sleep(25 * time.Millisecond)
	}
	j, _ := q.Job(id)
	t.Fatalf("job %d never reached %q (now %q, error %q)", id, want, j.State, j.Error)
	return nil
}

func TestQueueRetriesThenSucceeds(t *testing.T) {
	q, _, _ := newTestQueue(t)
	var calls int32
	q.Handle("flaky", func(ctx context.Context, j *Job, _ func(any)) error {
		if atomic.AddInt32(&calls, 1) < 3 {
			return errors.New("temporary")
		}
		return nil
	})
	q.Handle("fatal", func(context.Context, *Job, func(any)) error { return errors.Join(errors.New("bad config"), ErrNoRetry) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	a := &Job{Kind: "flaky"}
	b := &Job{Kind: "fatal"}
	q.Enqueue(a)
	q.Enqueue(b)
	if j := waitState(t, q, a.ID, "done"); j.Attempt != 3 {
		t.Errorf("expected 3 attempts, got %d", j.Attempt)
	}
	if j := waitState(t, q, b.ID, "failed"); j.Attempt != 1 {
		t.Errorf("a non-retryable error was retried (%d attempts)", j.Attempt)
	}
}

func TestQueueOneJobPerTargetAndCancel(t *testing.T) {
	q, _, _ := newTestQueue(t)
	var running, peak int32
	release := make(chan struct{})
	q.Handle("slow", func(ctx context.Context, j *Job, _ func(any)) error {
		n := atomic.AddInt32(&running, 1)
		if n > atomic.LoadInt32(&peak) {
			atomic.StoreInt32(&peak, n)
		}
		defer atomic.AddInt32(&running, -1)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	a, b, c := &Job{Kind: "slow", TargetID: 7}, &Job{Kind: "slow", TargetID: 7}, &Job{Kind: "slow", TargetID: 7}
	q.Enqueue(a)
	q.Enqueue(b)
	q.Enqueue(c)
	waitState(t, q, a.ID, "running")
	time.Sleep(300 * time.Millisecond)
	if j, _ := q.Job(b.ID); j.State != "queued" {
		t.Fatalf("second job on the same target is %q, want queued", j.State)
	}
	if err := q.Cancel(c.ID); err != nil { // cancel a waiting job
		t.Fatal(err)
	}
	if err := q.Cancel(a.ID); err != nil { // cancel a running job
		t.Fatal(err)
	}
	waitState(t, q, a.ID, "cancelled")
	waitState(t, q, b.ID, "running")
	close(release)
	waitState(t, q, b.ID, "done")
	waitState(t, q, c.ID, "cancelled")
	if peak != 1 {
		t.Errorf("%d jobs ran on one target at the same time", peak)
	}
}

func TestQueueRecoversAfterCrashAndHold(t *testing.T) {
	q, st, path := newTestQueue(t)
	started := make(chan struct{})
	q.Handle("work", func(ctx context.Context, j *Job, _ func(any)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	j := &Job{Kind: "work"}
	q.Enqueue(j)
	<-started
	cancel() // the manager "dies" while the job runs
	time.Sleep(200 * time.Millisecond)
	if got, _ := q.Job(j.ID); got.State != "running" {
		t.Fatalf("after a crash the job should still be marked running, got %q", got.State)
	}
	st.DB.Close()

	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	q2 := NewQueue(st2, NewHub(), 1)
	q2.Handle("work", func(context.Context, *Job, func(any)) error { return nil })
	q2.Recover()
	if got, _ := q2.Job(j.ID); got.State != "queued" {
		t.Fatalf("recovered job is %q, want queued", got.State)
	}
	q2.Hold(true)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go q2.Run(ctx2)
	time.Sleep(400 * time.Millisecond)
	if got, _ := q2.Job(j.ID); got.State != "queued" {
		t.Fatalf("job started during a hold (%q)", got.State)
	}
	q2.Hold(false)
	if got := waitState(t, q2, j.ID, "done"); got.Attempt != 2 {
		t.Errorf("expected attempt 2 after recovery, got %d", got.Attempt)
	}
}

func TestSchedule(t *testing.T) {
	if n, err := NextRun("", time.Now()); n != 0 || err != nil {
		t.Error("manual plan must have no next run")
	}
	from := time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC)
	if n, _ := NextRun("@every 6h", from); n != from.Add(6*time.Hour).Unix() {
		t.Error("@every 6h wrong")
	}
	if n, _ := NextRun("CRON_TZ=UTC 0 3 * * *", from); n != time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC).Unix() {
		t.Error("cron wrong")
	}
	for _, bad := range []string{"nonsense", "* * * * *", "@every 30s"} {
		if _, err := NextRun(bad, from); err == nil {
			t.Errorf("schedule %q was accepted", bad)
		}
	}
}

func TestRetentionKeepsChainsWhole(t *testing.T) {
	now := int64(1_800_000_000)
	day := int64(86400)
	mk := func(id, chain string, ageDays int64, pinned bool) *Backup {
		return &Backup{ID: id, ChainID: chain, CreatedAt: now - ageDays*day, Status: "done", Pinned: pinned}
	}
	ids := func(l []*Backup) string {
		var s []string
		for _, b := range l {
			s = append(s, b.ID)
		}
		return strings.Join(s, ",")
	}
	// chain A: full 30 days old with incrementals at 25 and 10 days; chain B: full 5 days, incremental 1 day
	list := []*Backup{mk("A0", "A0", 30, false), mk("A1", "A0", 25, false), mk("A2", "A0", 10, false), mk("B0", "B0", 5, false), mk("B1", "B0", 1, false), mk("L", "", 40, false)}
	// 14 days: A's newest member is 10 days old, so the 30-day-old full must stay with it
	if got := ids(RetentionVictims(list, 14, 0, now)); got != "L" {
		t.Errorf("a full backup was separated from a younger incremental: %s", got)
	}
	// 7 days: the whole chain A goes, newest member first
	if got := ids(RetentionVictims(list, 7, 0, now)); got != "A2,A1,A0,L" {
		t.Errorf("chain not removed as a whole, end first: %s", got)
	}
	// keep last 1 group: chain B stays complete
	if got := ids(RetentionVictims(list, 0, 1, now)); got != "A2,A1,A0,L" {
		t.Errorf("keep last 1: %s", got)
	}
	// a pinned incremental protects its whole chain
	list[1].Pinned = true
	if got := ids(RetentionVictims(list, 7, 0, now)); got != "L" {
		t.Errorf("pinned member did not protect its chain: %s", got)
	}
}

func TestRunnerAndSpecMethods(t *testing.T) {
	for _, bad := range []string{"", "docker:", "docker:a b", "docker:x;rm -rf /", "ssh:host"} {
		if _, err := NewRunner(bad); err == nil {
			t.Errorf("run mode %q was accepted", bad)
		}
	}
	r, err := NewRunner("docker:db-replica-1")
	if err != nil {
		t.Fatal(err)
	}
	cmd := r.Cmd(context.Background(), "s3cret-pw", false, "mariabackup", "--backup")
	if strings.Contains(strings.Join(cmd.Args, " "), "s3cret-pw") {
		t.Error("the password is on the command line")
	}
	phys := Spec{Method: "physical", Tables: []string{"a.b"}}
	if phys.Normalize() == nil {
		t.Error("a physical plan with chosen tables was accepted")
	}
	if (&Spec{Method: "tape"}).Normalize() == nil {
		t.Error("unknown method accepted")
	}
	ok := Spec{Method: "physical", IncrementalsPerFull: 6}
	if err := ok.Normalize(); err != nil || ok.ScopeText() != "whole server (data files)" {
		t.Error("physical spec:", err, ok.ScopeText())
	}
	if m := reBinlog.FindStringSubmatch("binlog_pos = filename 'binlog.000003', position '4711', GTID of the last change '0-99-12'"); m == nil || m[1] != "binlog.000003" || m[2] != "4711" || m[3] != "0-99-12" {
		t.Error("binlog position not parsed", m)
	}
}

func TestRowSearchSQLIsParameterised(t *testing.T) {
	cols := []RowColumn{{Name: "id", Type: "int", Key: true}, {Name: "na`me", Type: "varchar"}, {Name: "photo", Type: "blob"}}
	evil := "x' OR 1=1; DROP TABLE users; --"
	where, args, order, err := buildRowSQL(RowQuery{Search: evil, Sort: "na`me", Dir: "desc",
		Filters: []RowFilter{{Column: "id", Op: "ge", Value: evil}, {Column: "na`me", Op: "contains", Value: "50%_off"}, {Column: "photo", Op: "null"}}}, cols)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(where+order, "DROP") || strings.Contains(where+order, "1=1") {
		t.Fatalf("a typed value reached the SQL text: %s %s", where, order)
	}
	want := " WHERE `id` >= ? AND `na``me` LIKE ? AND `photo` IS NULL AND (CAST(`id` AS CHAR) LIKE ? OR CAST(`na``me` AS CHAR) LIKE ?)"
	if where != want {
		t.Errorf("where:\n got %s\nwant %s", where, want)
	}
	if order != " ORDER BY `na``me` DESC" {
		t.Errorf("order: %s", order)
	}
	if len(args) != 4 || args[0] != evil || args[1] != `%50\%\_off%` {
		t.Errorf("args: %#v", args)
	}
	for _, q := range []RowQuery{{Sort: "nope"}, {Filters: []RowFilter{{Column: "id; DROP", Op: "eq"}}}, {Filters: []RowFilter{{Column: "id", Op: "= 1 OR"}}}} {
		if _, _, _, err := buildRowSQL(q, cols); err == nil {
			t.Errorf("accepted %+v", q)
		}
	}
	if _, _, order, _ := buildRowSQL(RowQuery{}, cols); order != " ORDER BY `id`" {
		t.Errorf("default order should be the primary key, got %q", order)
	}
	if v := cellValue(kindBinary, sql.RawBytes{0, 255}); v != "0x00ff" {
		t.Errorf("binary cell: %v", v)
	}
	if v := cellValue(kindText, nil); v != nil {
		t.Errorf("NULL cell: %v", v)
	}
}

func TestTableRestoreRewrite(t *testing.T) {
	cases := []struct{ in, want string }{
		{"CREATE TABLE `cards` (\n", "CREATE TABLE `cards__r` (\n"},
		{"INSERT INTO `cards` (`id`,`x`) VALUES\n", "INSERT INTO `cards__r` (`id`,`x`) VALUES\n"},
		{"(1,'INSERT INTO `cards` (x) hello'),\n", "(1,'INSERT INTO `cards` (x) hello'),\n"}, // row data is left alone
		{"  CONSTRAINT `fk_from` FOREIGN KEY (`a`) REFERENCES `accounts` (`id`)\n", "  CONSTRAINT `fk_from_r1` FOREIGN KEY (`a`) REFERENCES `accounts` (`id`)\n"},
		{"  `cards` int(11) NOT NULL,\n", "  `cards` int(11) NOT NULL,\n"}, // a column with the table's name
	}
	for _, c := range cases {
		got, skip := rewriteTableSQL(c.in, "cards", "cards__r", "_r1")
		if skip || got != c.want {
			t.Errorf("%q -> %q (skip %v), want %q", c.in, got, skip, c.want)
		}
	}
	for _, drop := range []string{"DROP TABLE IF EXISTS `cards`;\n", "-- comment\n", "/*!40101 SET NAMES utf8mb4 */;\n"} {
		if _, skip := rewriteTableSQL(drop, "cards", "cards__r", "_r1"); !skip {
			t.Errorf("%q must be skipped", drop)
		}
	}
	// another table's statements are not renamed, so the handler rejects them instead of running them
	if got, _ := rewriteTableSQL("INSERT INTO `users` (`id`) VALUES\n", "cards", "cards__r", "_r1"); got != "INSERT INTO `users` (`id`) VALUES\n" {
		t.Errorf("foreign statement was rewritten: %q", got)
	}
}

func TestSearchSkipsColumnsThatAreNotText(t *testing.T) {
	cols := []RowColumn{{Name: "id", Type: "int"}, {Name: "pt", Type: "point"}, {Name: "bl", Type: "longblob"}, {Name: "js", Type: "longtext"}}
	where, args, _, _ := buildRowSQL(RowQuery{Search: "x"}, cols)
	if strings.Contains(where, "`pt`") || strings.Contains(where, "`bl`") || len(args) != 2 {
		t.Errorf("geometry / binary columns must not be text-searched: %s", where)
	}
}

func TestRedisSnapshotFormats(t *testing.T) {
	body := bytes.Repeat([]byte("REDIS-rdb-bytes\r\n$5\r\n"), 5000)
	// 1. length-prefixed, with keep-alive new lines before it and replication traffic after it
	var out bytes.Buffer
	in := append([]byte("\n\n$"+fmt.Sprint(len(body))+"\r\n"), body...)
	in = append(in, []byte("*1\r\n$4\r\nPING\r\n")...)
	if n, err := readRDB(bufio.NewReader(bytes.NewReader(in)), &out); err != nil || n != int64(len(body)) || !bytes.Equal(out.Bytes(), body) {
		t.Fatalf("length format: n=%d err=%v", n, err)
	}
	// 2. diskless: ends with a 40-byte marker, read in small pieces so the marker straddles reads
	mark := strings.Repeat("ab12", 10)
	out.Reset()
	in = append([]byte("$EOF:"+mark+"\r\n"), body...)
	in = append(in, []byte(mark)...)
	if n, err := readRDB(bufio.NewReaderSize(iotest7{bytes.NewReader(in)}, 16), &out); err != nil || n != int64(len(body)) || !bytes.Equal(out.Bytes(), body) {
		t.Fatalf("marker format: n=%d err=%v", n, err)
	}
	// 3. a cut stream is an error, never a short "good" snapshot
	out.Reset()
	if _, err := readRDB(bufio.NewReader(bytes.NewReader(in[:len(in)-10])), &out); err == nil {
		t.Fatal("a cut marker stream was accepted")
	}
	if _, err := readRDB(bufio.NewReader(strings.NewReader("$1000\r\nshort")), &out); err == nil {
		t.Fatal("a cut length stream was accepted")
	}
	if _, err := readRDB(bufio.NewReader(strings.NewReader("-NOPERM no\r\n")), &out); err == nil {
		t.Fatal("an error reply was accepted")
	}
}

type iotest7 struct{ r io.Reader }

func (o iotest7) Read(p []byte) (int, error) {
	if len(p) > 7 {
		p = p[:7]
	}
	return o.r.Read(p)
}

func TestFileExcludes(t *testing.T) {
	pats := []string{"node_modules", "*.log", "/srv/app/cache", ""}
	for full, want := range map[string]bool{"/srv/app/node_modules": true, "/srv/app/x/debug.log": true, "/srv/app/cache": true, "/srv/app/cache/a/b": true,
		"/srv/app/cachet": false, "/srv/app/src/main.go": false, "/srv/app/logs": false} {
		if got := excluded(pats, full, filepath.Base(full)); got != want {
			t.Errorf("excluded(%s) = %v, want %v", full, got, want)
		}
	}
	if MethodForKind("files") != "files" || MethodForKind("mysql") != "" {
		t.Error("MethodForKind")
	}
	s := Spec{Method: "redis", Tables: []string{"a.b"}}
	if err := s.Normalize(); err != nil || len(s.Tables) != 0 {
		t.Error("redis spec must drop table choices", err)
	}
}
