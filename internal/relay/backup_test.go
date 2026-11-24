package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// backupEnv points the backup settings at a fresh data dir and returns it.
func backupEnv(t *testing.T) (dataDir string, cfg backupConfig) {
	t.Helper()
	clearSettings(t)
	t.Setenv("DATABASE_URL", "")
	dataDir = filepath.Join(t.TempDir(), "data")
	t.Setenv("LANGSTROTH_RELAY_DATA_DIR", dataDir)
	cfg, err := backupConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return dataDir, cfg
}

// seededStore is a persisted memory store holding one workspace with an
// envelope and an owner, flushed to relay-state.json.
func seededStore(t *testing.T, dataDir, ws string) *memoryStore {
	t.Helper()
	s := newMemoryStoreWithPersistence(dataDir)
	if _, err := s.AppendEnvelope(bg, ws, json.RawMessage(`{"c":"ciphertext"}`), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimWorkspace(bg, ws, "acct-owner", "owner", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(bg); err != nil {
		t.Fatal(err)
	}
	return s
}

// clockAt makes a runner whose backups are named at successive seconds.
func clockAt(r *backupRunner, start time.Time) {
	n := 0
	r.now = func() time.Time { n++; return start.Add(time.Duration(n) * time.Second) }
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestBackupConfigDefaultsAndErrors(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	if !cfg.enabled || !cfg.restore || cfg.interval != time.Hour || cfg.keep != 24 || cfg.dir != filepath.Join(dataDir, "backups") || !cfg.applies() {
		t.Fatalf("defaults: %+v", cfg)
	}
	t.Setenv("DATABASE_URL", "postgres://x")
	if cfg.applies() {
		t.Error("backups must not apply to Postgres")
	}
	t.Setenv("DATABASE_URL", "")
	for name, bad := range map[string]string{
		"LANGSTROTH_RELAY_BACKUP":          "maybe",
		"LANGSTROTH_RELAY_BACKUP_RESTORE":  "sometimes",
		"LANGSTROTH_RELAY_BACKUP_INTERVAL": "10s",
		"LANGSTROTH_RELAY_BACKUP_KEEP":     "0",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, bad)
			if err := backupConfigError(); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s=%s: want a config error naming it, got %v", name, bad, err)
			}
			if strings.Contains(fmt.Sprint(backupConfigError()), "postgres://") {
				t.Error("error leaks another setting")
			}
		})
	}
	t.Setenv("LANGSTROTH_RELAY_BACKUP", "off")
	t.Setenv("LANGSTROTH_RELAY_BACKUP_INTERVAL", "30m")
	t.Setenv("LANGSTROTH_RELAY_BACKUP_KEEP", "3")
	t.Setenv("LANGSTROTH_RELAY_BACKUP_DIR", "/elsewhere")
	cfg, err := backupConfigFromEnv()
	if err != nil || cfg.enabled || cfg.interval != 30*time.Minute || cfg.keep != 3 || cfg.dir != "/elsewhere" {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}
}

// A backup is the flushed snapshot, gzipped, owner-only; an unchanged or
// empty state adds no backup; rotation keeps the newest N and nothing else
// is touched.
func TestBackupOnceWritesRotatesAndSkips(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	cfg.keep = 3
	r := newBackupRunner(cfg)
	clockAt(r, time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC))

	// Nothing flushed yet, then an empty state: no backup.
	if name, err := r.backupOnce(); err != nil || name != "" {
		t.Fatalf("no snapshot: %q %v", name, err)
	}
	empty := newMemoryStoreWithPersistence(dataDir)
	if err := empty.Flush(bg); err != nil {
		t.Fatal(err)
	}
	if name, err := r.backupOnce(); err != nil || name != "" {
		t.Fatalf("empty state must be skipped: %q %v", name, err)
	}

	s := seededStore(t, dataDir, "ws-a")
	name, err := r.backupOnce()
	if err != nil || name == "" {
		t.Fatalf("first backup: %q %v", name, err)
	}
	if m := modeOf(t, cfg.dir); m != 0o700 {
		t.Errorf("backup dir mode %04o, want 0700", m)
	}
	if m := modeOf(t, filepath.Join(cfg.dir, name)); m != 0o600 {
		t.Errorf("backup file mode %04o, want 0600", m)
	}
	state, _ := os.ReadFile(filepath.Join(dataDir, snapshotFile))
	raw, err := readBackup(filepath.Join(cfg.dir, name), maxRestoreBytes())
	if err != nil || !bytes.Equal(raw, state) {
		t.Fatalf("backup is not the snapshot: %v", err)
	}

	// Unchanged: skipped, also by a fresh runner (the hash comes from disk).
	if n, _ := r.backupOnce(); n != "" {
		t.Errorf("unchanged state backed up again as %s", n)
	}
	r2 := newBackupRunner(cfg)
	clockAt(r2, time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC))
	if n, _ := r2.backupOnce(); n != "" {
		t.Errorf("fresh runner backed up an unchanged state as %s", n)
	}

	// A file that only looks like a backup, and an unrelated one, survive rotation.
	keepers := []string{"relay-backup-notatime.json.gz", "notes.txt"}
	for _, k := range keepers {
		if err := os.WriteFile(filepath.Join(cfg.dir, k), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := s.AppendEnvelope(bg, "ws-a", json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)), ""); err != nil {
			t.Fatal(err)
		}
		if err := s.Flush(bg); err != nil {
			t.Fatal(err)
		}
		if n, err := r.backupOnce(); err != nil || n == "" {
			t.Fatalf("backup %d: %q %v", i, n, err)
		}
	}
	list, _ := listBackups(cfg.dir)
	if len(list) != 3 {
		t.Fatalf("kept %d backups, want 3: %+v", len(list), list)
	}
	for _, k := range keepers {
		if _, err := os.Stat(filepath.Join(cfg.dir, k)); err != nil {
			t.Errorf("rotation removed %s", k)
		}
	}
	// No temp files are left behind.
	ents, _ := os.ReadDir(cfg.dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}

	// A wipe (the store emptied and flushed) never rotates good backups away.
	wiped := newMemoryStore()
	wiped.persistPath = filepath.Join(dataDir, snapshotFile)
	if err := wiped.Flush(bg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if n, _ := r.backupOnce(); n != "" {
			t.Fatalf("an empty state was backed up as %s", n)
		}
	}
	if after, _ := listBackups(cfg.dir); len(after) != 3 || after[2].Name != list[2].Name {
		t.Errorf("backups changed after a wipe: %+v", after)
	}
}

// The full loop: a relay's state survives losing relay-state.json, and a
// damaged relay-state.json is kept aside rather than overwritten.
func TestRestoreOnBoot(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt", "empty-file"} {
		t.Run(damage, func(t *testing.T) {
			dataDir, cfg := backupEnv(t)
			seededStore(t, dataDir, "ws-restore")
			if _, err := newBackupRunner(cfg).backupOnce(); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(dataDir, snapshotFile)
			switch damage {
			case "missing":
				os.Remove(state)
			case "corrupt":
				os.WriteFile(state, []byte(`{"workspaces": {`), 0o600)
			case "empty-file":
				os.WriteFile(state, nil, 0o600)
			}
			if err := prepareDataDir(cfg); err != nil {
				t.Fatal(err)
			}
			s := newMemoryStoreWithPersistence(dataDir)
			envs, err := s.EnvelopesAfter(bg, "ws-restore", 0)
			if err != nil || len(envs) != 1 {
				t.Fatalf("restored store has %d envelopes (%v)", len(envs), err)
			}
			if members, _ := s.WorkspaceMembers(bg, "ws-restore"); len(members) != 1 {
				t.Errorf("roster not restored: %+v", members)
			}
			if m := modeOf(t, state); m != 0o600 {
				t.Errorf("restored snapshot mode %04o", m)
			}
			aside, _ := filepath.Glob(state + ".unreadable-*")
			if damage == "missing" && len(aside) != 0 || damage != "missing" && len(aside) != 1 {
				t.Errorf("moved aside: %v", aside)
			}
		})
	}
}

// A snapshot that reads is never replaced, and restore off starts empty.
func TestRestoreOnBootNeverClobbersAndCanBeOff(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	seededStore(t, dataDir, "ws-old")
	if _, err := newBackupRunner(cfg).backupOnce(); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dataDir, snapshotFile)
	os.Remove(state)
	seededStore(t, dataDir, "ws-new") // a newer, readable state
	before, _ := os.ReadFile(state)
	if err := prepareDataDir(cfg); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(state); !bytes.Equal(before, after) {
		t.Fatal("a readable snapshot was replaced")
	}

	os.Remove(state)
	cfg.restore = false
	if err := prepareDataDir(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restore off still restored")
	}
}

// Restore refuses planted or tampered backups and falls back to the newest
// one that passes; with none, it starts empty.
func TestRestoreRefusesTamperedBackups(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	seededStore(t, dataDir, "ws-good")
	r := newBackupRunner(cfg)
	clockAt(r, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	good, err := r.backupOnce()
	if err != nil || good == "" {
		t.Fatal(err)
	}
	newer := func(i int) string { return backupName(time.Date(2026, 6, 1, 0, 0, i, 0, time.UTC)) }

	// Every one of these is newer than the good backup and must be skipped.
	elsewhere := filepath.Join(t.TempDir(), "evil.json.gz")
	evil, _ := gzipBytes([]byte(`{"workspaces":{"ws-evil":{"envelopes":[],"nextSeq":0,"candidates":{},"presence":{},"keyring":[]}}}`))
	os.WriteFile(elsewhere, evil, 0o600)
	if err := os.Symlink(elsewhere, filepath.Join(cfg.dir, newer(1))); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfg.dir, newer(2)), evil, 0o666)
	os.Chmod(filepath.Join(cfg.dir, newer(2)), 0o666) // group/other writable
	os.WriteFile(filepath.Join(cfg.dir, newer(3)), []byte("not gzip"), 0o600)
	notJSON, _ := gzipBytes([]byte("not json"))
	os.WriteFile(filepath.Join(cfg.dir, newer(4)), notJSON, 0o600)
	os.Mkdir(filepath.Join(cfg.dir, newer(5)), 0o700)
	t.Setenv("LANGSTROTH_RELAY_MAX_STORE_BYTES", "1024")
	if got := maxRestoreBytes(); got != 4*1024+256<<20 {
		t.Errorf("limit with a store cap: %d", got)
	}
	restoreLimit = func() int64 { return 64 << 10 }
	defer func() { restoreLimit = maxRestoreBytes }()
	bomb := gzipBomb(t, restoreLimit()+1)
	os.WriteFile(filepath.Join(cfg.dir, newer(6)), bomb, 0o600)
	if _, err := readBackup(filepath.Join(cfg.dir, newer(6)), restoreLimit()); err == nil || !strings.Contains(err.Error(), "decompresses to more than") {
		t.Errorf("gzip bomb: %v", err)
	}

	os.Remove(filepath.Join(dataDir, snapshotFile))
	b, err := restoreNewest(cfg, time.Now())
	if name := b.Name; err != nil || name != good {
		t.Fatalf("restored %q (%v), want the good backup %q", b.Name, err, good)
	}
	s := newMemoryStoreWithPersistence(dataDir)
	if envs, _ := s.EnvelopesAfter(bg, "ws-good", 0); len(envs) != 1 {
		t.Error("good backup not restored")
	}
	if envs, _ := s.EnvelopesAfter(bg, "ws-evil", 0); len(envs) != 0 || len(s.workspaces) != 1 {
		t.Error("a tampered backup was restored")
	}

	// With only bad backups: nothing restored, an error (the relay refuses to start).
	os.Remove(filepath.Join(cfg.dir, good))
	os.Remove(filepath.Join(dataDir, snapshotFile))
	if b, err := restoreNewest(cfg, time.Now()); err == nil || b.Name != "" {
		t.Fatalf("only bad backups: %q %v", b.Name, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, snapshotFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("something was installed from a bad backup")
	}
}

// gzipBomb is a gzip stream that decompresses to n bytes of JSON-ish zeros.
func gzipBomb(t *testing.T, n int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	chunk := bytes.Repeat([]byte(" "), 4<<10)
	for written := int64(0); written < n; written += int64(len(chunk)) {
		gz.Write(chunk)
	}
	gz.Close()
	return buf.Bytes()
}

// A backup directory others can write is refused for writing and restoring;
// a symlinked one too.
func TestBackupDirSafety(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	seededStore(t, dataDir, "ws")
	if n, err := newBackupRunner(cfg).backupOnce(); err != nil || n == "" {
		t.Fatal(err)
	}
	os.Chmod(cfg.dir, 0o777)
	os.Remove(filepath.Join(dataDir, snapshotFile))
	seededStore(t, dataDir, "ws2")
	if _, err := newBackupRunner(cfg).backupOnce(); !errors.Is(err, errUnsafe) {
		t.Fatalf("world-writable dir: %v", err)
	}
	if _, err := restoreNewest(cfg, time.Now()); !errors.Is(err, errUnsafe) {
		t.Fatalf("restore from a world-writable dir: %v", err)
	}
	backupCheck(func(id, status, detail, fix string) {
		if status != "fail" || fix == "" {
			t.Errorf("check on an unsafe dir: %s %s %s", status, detail, fix)
		}
	})
	os.Chmod(cfg.dir, 0o700)

	real := filepath.Join(t.TempDir(), "real")
	os.Mkdir(real, 0o700)
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(real, link)
	cfg.dir = link
	if _, err := newBackupRunner(cfg).backupOnce(); !errors.Is(err, errUnsafe) {
		t.Fatalf("symlinked dir: %v", err)
	}
}

func runRestore(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := cmdRestore(args, &b)
	return b.String(), err
}

// ageSnapshot makes relay-state.json look like no relay is writing it.
func ageSnapshot(t *testing.T, dataDir string) {
	t.Helper()
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(dataDir, snapshotFile), old, old); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreCommand(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	s := seededStore(t, dataDir, "ws-1")
	r := newBackupRunner(cfg)
	clockAt(r, time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC))
	first, _ := r.backupOnce()
	s.AppendEnvelope(bg, "ws-1", json.RawMessage(`{"second":true}`), "")
	s.Flush(bg)
	if _, err := r.backupOnce(); err != nil {
		t.Fatal(err)
	}

	// A running relay (fresh relay-state.json) is refused.
	_, err := runRestore(t)
	var p *problem
	if !errors.As(err, &p) || p.Code != "running" || p.exit != exitFailed || p.Fix == "" {
		t.Fatalf("running relay: %v", err)
	}
	ageSnapshot(t, dataDir)

	// Names that could climb out of the backup dir are usage errors.
	for _, bad := range []string{"..", "relay-backup-x.json.gz", "relay-state.json"} {
		if _, err := runRestore(t, "--from", bad); !errors.Is(err, errUsage) {
			t.Errorf("--from %q: %v", bad, err)
		}
	}
	if _, err := runRestore(t, "--bogus"); !errors.Is(err, errUsage) {
		t.Errorf("unknown flag: %v", err)
	}

	// Restore the older backup by name, as JSON.
	out, err := runRestore(t, "--from="+first, "--json")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	var rep checkReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Schema != checksSchema || rep.Command != "restore" || !rep.OK || len(rep.Checks) != 3 {
		t.Fatalf("report: %s (%v)", out, err)
	}
	aside, _ := filepath.Glob(filepath.Join(dataDir, snapshotFile+".before-restore-*"))
	if len(aside) != 1 {
		t.Fatalf("previous state not kept: %v", aside)
	}
	s2 := newMemoryStoreWithPersistence(dataDir)
	if envs, _ := s2.EnvelopesAfter(bg, "ws-1", 0); len(envs) != 1 {
		t.Errorf("restored %d envelopes, want the older backup's 1", len(envs))
	}

	// A path from elsewhere is held to the same file checks.
	copyPath := filepath.Join(t.TempDir(), "copy.json.gz")
	data, _ := os.ReadFile(filepath.Join(cfg.dir, first))
	os.WriteFile(copyPath, data, 0o644)
	os.Chmod(copyPath, 0o664)
	ageSnapshot(t, dataDir)
	if _, err := runRestore(t, "--from", copyPath); !errors.As(err, &p) || p.Code != "unsafe" {
		t.Errorf("group-writable copy: %v", err)
	}
	os.Chmod(copyPath, 0o600)
	if out, err := runRestore(t, "--from", copyPath); err != nil || !strings.Contains(out, "restored") {
		t.Errorf("copy restore: %q %v", out, err)
	}

	// Postgres and a missing data dir are refused.
	t.Setenv("DATABASE_URL", "postgres://u:secretpw@db/relay")
	if _, err := runRestore(t); !errors.As(err, &p) || p.Code != "postgres" || strings.Contains(p.Message, "secretpw") {
		t.Errorf("postgres: %v", err)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LANGSTROTH_RELAY_DATA_DIR", "")
	if _, err := runRestore(t); !errors.Is(err, errUsage) {
		t.Errorf("no data dir: %v", err)
	}
}

// The check command's backup line.
func TestBackupCheck(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	line := func() checkResult {
		rep := runChecks(os.Environ(), "")
		for _, c := range rep.Checks {
			if c.ID == "backup" {
				return c
			}
		}
		t.Fatal("no backup check")
		return checkResult{}
	}
	if c := line(); c.Status != "ok" || !strings.Contains(c.Detail, "no backups yet") {
		t.Errorf("fresh: %+v", c)
	}
	seededStore(t, dataDir, "ws")
	newBackupRunner(cfg).backupOnce()
	if c := line(); c.Status != "ok" || !strings.Contains(c.Detail, "1 backups") {
		t.Errorf("one backup: %+v", c)
	}
	t.Setenv("LANGSTROTH_RELAY_BACKUP", "off")
	if c := line(); c.Status != "warn" {
		t.Errorf("off: %+v", c)
	}
	t.Setenv("LANGSTROTH_RELAY_BACKUP", "bogus")
	if c := line(); c.Status != "fail" {
		t.Errorf("bad value: %+v", c)
	}
	t.Setenv("LANGSTROTH_RELAY_BACKUP", "")
	t.Setenv("DATABASE_URL", "postgres://x")
	if c := line(); c.Status != "skip" {
		t.Errorf("postgres: %+v", c)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LANGSTROTH_RELAY_DATA_DIR", "")
	if c := line(); c.Status != "skip" {
		t.Errorf("memory only: %+v", c)
	}
}

// Flushing (which backups copy) while clients write must not race: the
// snapshot shares maps with the live store. Run with -race.
func TestFlushWhileWriting(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	s := seededStore(t, dataDir, "ws-live")
	r := newBackupRunner(cfg)
	ctx, cancel := context.WithTimeout(bg, 300*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			dev := fmt.Sprintf("dev-%d", i%50)
			_ = s.PutCandidate(bg, "ws-live", dev, json.RawMessage(`{"a":1}`))
			_ = s.PutPresence(bg, "ws-live", dev, json.RawMessage(`{"p":1}`))
			_ = s.UpsertMember(bg, "ws-live", fmt.Sprintf("acct-%d", i%50), "m", "member", "acct-owner", int64(i))
			_, _ = s.AppendEnvelope(bg, "ws-live", json.RawMessage(`{"x":1}`), "")
		}
	}()
	for ctx.Err() == nil {
		if err := s.Flush(bg); err != nil {
			t.Fatal(err)
		}
		if _, err := r.backupOnce(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

// A backup named in the future (a wrong clock, or a renamed old backup) is
// never restored and never wins; a clock that steps back doesn't make
// rotation delete the backup it just took.
func TestBackupClockSafety(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	cfg.keep = 1
	s := seededStore(t, dataDir, "ws-old")
	r := newBackupRunner(cfg)
	r.now = func() time.Time { return time.Now().Add(5 * 365 * 24 * time.Hour) }
	future, err := r.backupOnce()
	if err != nil || future == "" {
		t.Fatal(err)
	}
	s.AppendEnvelope(bg, "ws-new", json.RawMessage(`{"n":1}`), "")
	s.Flush(bg)
	r.now = time.Now
	now, err := r.backupOnce()
	if err != nil || now == "" {
		t.Fatalf("backup after the clock stepped back: %q %v", now, err)
	}
	for _, n := range []string{future, now} {
		if _, err := os.Stat(filepath.Join(cfg.dir, n)); err != nil {
			t.Errorf("%s was rotated away", n)
		}
	}
	os.Remove(filepath.Join(dataDir, snapshotFile))
	if err := prepareDataDir(cfg); err != nil {
		t.Fatal(err)
	}
	if envs, _ := newMemoryStoreWithPersistence(dataDir).EnvelopesAfter(bg, "ws-new", 0); len(envs) != 1 {
		t.Error("restore picked the future-named backup")
	}
	var c checkResult
	backupCheck(func(id, status, detail, fix string) {
		c = checkResult{ID: id, Status: status, Detail: detail, Fix: fix}
	})
	if c.Status != "warn" || !strings.Contains(c.Detail, "future") {
		t.Errorf("check: %+v", c)
	}
}

// Backups exist but none passes: the relay refuses to start rather than
// serve an empty store (rosters unclaimed). restore off starts empty.
func TestRestoreOnBootRefusesWhenEveryBackupFails(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	seededStore(t, dataDir, "ws")
	name, _ := newBackupRunner(cfg).backupOnce()
	os.Chmod(filepath.Join(cfg.dir, name), 0o660)
	os.Remove(filepath.Join(dataDir, snapshotFile))
	err := prepareDataDir(cfg)
	var p *problem
	if !errors.As(err, &p) || p.Fix == "" || !strings.Contains(p.Fix, "LANGSTROTH_RELAY_BACKUP_RESTORE=off") {
		t.Fatalf("want a refusal with a fix, got %v", err)
	}
	cfg.restore = false
	if err := prepareDataDir(cfg); err != nil {
		t.Errorf("restore off: %v", err)
	}
	// No backups at all: start empty, no error.
	cfg.restore = true
	os.Remove(filepath.Join(cfg.dir, name))
	if err := prepareDataDir(cfg); err != nil {
		t.Errorf("no backups: %v", err)
	}
}

// Whatever parseSnapshot accepts, the store loader accepts: a snapshot with
// trailing data is moved aside and restored, not loaded as empty.
func TestParseSnapshotMatchesLoader(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	seededStore(t, dataDir, "ws-trail")
	newBackupRunner(cfg).backupOnce()
	state := filepath.Join(dataDir, snapshotFile)
	raw, _ := os.ReadFile(state)
	os.WriteFile(state, append(raw, '}'), 0o600)
	if err := prepareDataDir(cfg); err != nil {
		t.Fatal(err)
	}
	if envs, _ := newMemoryStoreWithPersistence(dataDir).EnvelopesAfter(bg, "ws-trail", 0); len(envs) != 1 {
		t.Error("a snapshot with trailing data was kept and loaded as empty")
	}
	if aside, _ := filepath.Glob(state + ".unreadable-*"); len(aside) != 1 {
		t.Errorf("not moved aside: %v", aside)
	}
	// Errors name no map key.
	_, err := parseSnapshot([]byte(`{"tokens":{"tok-SECRET":{"hash":5}}}`))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("parse error leaks a key: %v", err)
	}
}

// The ticker's backup and the shutdown backup can overlap; they must not
// race (run with -race) or write the same file twice.
func TestConcurrentBackupOnce(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	seededStore(t, dataDir, "ws")
	r := newBackupRunner(cfg)
	var wg sync.WaitGroup
	names := make([]string, 4)
	for i := range names {
		wg.Add(1)
		go func(i int) { defer wg.Done(); names[i], _ = r.backupOnce() }(i)
	}
	wg.Wait()
	written := 0
	for _, n := range names {
		if n != "" {
			written++
		}
	}
	if written != 1 {
		t.Errorf("%d backups of one state, want 1: %v", written, names)
	}
}

// A large state is backed up without holding it in memory, and restores.
func TestBackupLargeStateStreams(t *testing.T) {
	dataDir, cfg := backupEnv(t)
	s := seededStore(t, dataDir, "ws-big")
	body := json.RawMessage(`{"c":"` + strings.Repeat("a", 64<<10) + `"}`)
	for i := 0; i < 64; i++ {
		s.AppendEnvelope(bg, "ws-big", body, "")
	}
	s.Flush(bg)
	name, err := newBackupRunner(cfg).backupOnce()
	if err != nil || name == "" {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(filepath.Join(dataDir, snapshotFile))
	got, err := readBackup(filepath.Join(cfg.dir, name), maxRestoreBytes())
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("round trip: %v", err)
	}
	sum, err := hashBackup(filepath.Join(cfg.dir, name), maxRestoreBytes())
	if err != nil || sum != sha256Sum(want) {
		t.Errorf("hash: %v", err)
	}
}

func sha256Sum(b []byte) [sha256.Size]byte { return sha256.Sum256(b) }
