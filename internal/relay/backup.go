package relay

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Built-in backups of the memory store (LANGSTROTH_RELAY_DATA_DIR).
//
// On an interval and on shutdown the relay copies its last written snapshot
// (relay-state.json) into a backup directory as a timestamped, gzipped file
// (owner-only), and keeps the newest LANGSTROTH_RELAY_BACKUP_KEEP of them.
// At startup, when relay-state.json is missing or unreadable, the newest
// backup that passes the checks below becomes relay-state.json again.
//
// Postgres (DATABASE_URL) is not covered: its own tooling (pg_dump,
// point-in-time recovery, CloudNativePG backups) is the right tool, and a
// second, partial export of the same tables would drift from the schema.
//
// What a backup trusts: a backup is the relay's own state, as trusted as
// relay-state.json. Restore only reads regular files (never a symlink) owned
// by the relay's user and not writable by group or others, with an exact
// file name, from a directory others can't write; it bounds the decompressed
// size and parses the result as a snapshot before anything is replaced. The
// file it replaces is moved aside, never deleted. Attachment bytes (blobs/)
// are not in the backup; the snapshot lists them with their SHA-256, and on
// load any whose bytes are missing or differ are dropped.
//
// Concurrency: the backup reads relay-state.json, which the flush loop
// replaces atomically (temp file and rename), so a backup is always one whole
// snapshot, at most one flush interval old. Nothing here touches the live
// maps.

const (
	defaultBackupInterval = time.Hour
	minBackupInterval     = time.Minute
	defaultBackupKeep     = 24
	maxBackupKeep         = 10000
	backupSubdir          = "backups"
	backupFilePrefix      = "relay-backup-"
	backupFileSuffix      = ".json.gz"
	backupTimeLayout      = "20060102T150405Z"
	// defaultMaxRestoreBytes bounds a backup's decompressed size when
	// LANGSTROTH_RELAY_MAX_STORE_BYTES is unset (a gzip bomb can't fill memory).
	defaultMaxRestoreBytes int64 = 4 << 30
	// runningWindow: relay-state.json written this recently means a relay is
	// running on the data dir (it rewrites the file every 5 s).
	runningWindow = 15 * time.Second
)

// backupNameRe is the only file name a backup may have: no path separators,
// no dots beyond the suffix, nothing a traversal could use.
var backupNameRe = regexp.MustCompile(`^relay-backup-[0-9]{8}T[0-9]{6}Z\.json\.gz$`)

// backupConfig is the backup settings, resolved from the environment.
type backupConfig struct {
	enabled  bool          // LANGSTROTH_RELAY_BACKUP (on with a data dir)
	restore  bool          // LANGSTROTH_RELAY_BACKUP_RESTORE
	dir      string        // LANGSTROTH_RELAY_BACKUP_DIR, default <data dir>/backups
	interval time.Duration // LANGSTROTH_RELAY_BACKUP_INTERVAL
	keep     int           // LANGSTROTH_RELAY_BACKUP_KEEP
	dataDir  string
}

// onOffSetting reads an on/off setting: unset is def; anything but the usual
// spellings is an error (a typo must not silently turn backups off).
func onOffSetting(name string, def bool) (bool, error) {
	switch v := strings.ToLower(strings.TrimSpace(getenv(name))); v {
	case "":
		return def, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return def, fmt.Errorf("%s=%q is not on or off", name, v)
	}
}

// backupConfigFromEnv resolves the backup settings. An error is a
// configuration mistake: the relay refuses to start on it.
func backupConfigFromEnv() (backupConfig, error) {
	observeDefault("LANGSTROTH_RELAY_BACKUP_KEEP", defaultBackupKeep)
	c := backupConfig{interval: defaultBackupInterval, keep: defaultBackupKeep}
	c.dataDir = getenv("LANGSTROTH_RELAY_DATA_DIR")
	var err error
	if c.enabled, err = onOffSetting("LANGSTROTH_RELAY_BACKUP", true); err != nil {
		return c, err
	}
	if c.restore, err = onOffSetting("LANGSTROTH_RELAY_BACKUP_RESTORE", true); err != nil {
		return c, err
	}
	if v := strings.TrimSpace(getenv("LANGSTROTH_RELAY_BACKUP_INTERVAL")); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d < minBackupInterval {
			return c, fmt.Errorf("LANGSTROTH_RELAY_BACKUP_INTERVAL=%q must be a duration of at least 1m (for example 30m, 1h, 24h)", v)
		}
		c.interval = d
	}
	if v := strings.TrimSpace(getenv("LANGSTROTH_RELAY_BACKUP_KEEP")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > maxBackupKeep {
			return c, fmt.Errorf("LANGSTROTH_RELAY_BACKUP_KEEP=%q must be a whole number from 1 to %d", v, maxBackupKeep)
		}
		c.keep = n
	}
	c.dir = strings.TrimSpace(getenv("LANGSTROTH_RELAY_BACKUP_DIR"))
	if c.dir == "" && c.dataDir != "" {
		c.dir = filepath.Join(c.dataDir, backupSubdir)
	}
	return c, nil
}

// applies reports whether built-in backups cover this process's store: the
// memory store with a data dir. Postgres and memory-only are not covered.
func (c backupConfig) applies() bool {
	return c.dataDir != "" && getenv("DATABASE_URL") == ""
}

// backupConfigError is the startup check: settings that don't parse stop the
// relay (like every other configuration error).
func backupConfigError() error {
	_, err := backupConfigFromEnv()
	return err
}

// maxRestoreBytes bounds a backup's decompressed size: a few times the store
// cap when one is set, else defaultMaxRestoreBytes.
func maxRestoreBytes() int64 {
	if n := envInt64Quiet("LANGSTROTH_RELAY_MAX_STORE_BYTES"); n > 0 {
		return 4*n + 256<<20
	}
	return defaultMaxRestoreBytes
}

// restoreLimit is the bound restores use (a variable so tests can shrink it).
var restoreLimit = maxRestoreBytes

// envInt64Quiet reads a non-negative integer without recording a default
// (the store's own reader records it).
func envInt64Quiet(name string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(getenv(name)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ── Directory and file safety ────────────────────────────────────────────────

// errUnsafe marks a backup directory or file the relay refuses to use.
var errUnsafe = errors.New("unsafe")

// ensureBackupDir creates dir (mode 0700) if needed and checks it is a real
// directory others can't write.
func ensureBackupDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkBackupDir(dir)
}

// checkBackupDir refuses a backup directory that is a symlink, not a
// directory, or writable by others (anyone could plant or swap a backup).
// Group write is allowed: Kubernetes volumes are often group-writable for
// fsGroup.
func checkBackupDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: backup directory %s is a symlink; point LANGSTROTH_RELAY_BACKUP_DIR at the real directory", errUnsafe, dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: backup directory %s is not a directory", errUnsafe, dir)
	}
	if fi.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%w: backup directory %s is writable by others (mode %04o); chmod o-w it", errUnsafe, dir, fi.Mode().Perm())
	}
	return nil
}

// checkBackupFile refuses to restore from anything but a regular file (not a
// symlink) owned by this process's user and not writable by group or others.
func checkBackupFile(path string) (fs.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symlink", errUnsafe, path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", errUnsafe, path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: %s is writable by group or others (mode %04o); chmod 600 it", errUnsafe, path, fi.Mode().Perm())
	}
	if !ownedByMe(fi) {
		return nil, fmt.Errorf("%w: %s is not owned by the relay's user", errUnsafe, path)
	}
	return fi, nil
}

// backupEntry is one backup file in the directory.
type backupEntry struct {
	Name  string
	Size  int64
	Taken time.Time
}

// listBackups returns the backups in dir, oldest first. Only exact backup
// names that are regular files count; anything else is left alone.
func listBackups(dir string) ([]backupEntry, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []backupEntry
	for _, e := range ents {
		name := e.Name()
		if !backupNameRe.MatchString(name) || !e.Type().IsRegular() {
			continue
		}
		taken, err := time.Parse(backupTimeLayout, strings.TrimSuffix(strings.TrimPrefix(name, backupFilePrefix), backupFileSuffix))
		if err != nil {
			continue
		}
		var size int64
		if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		out = append(out, backupEntry{Name: name, Size: size, Taken: taken})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func backupName(t time.Time) string {
	return backupFilePrefix + t.UTC().Format(backupTimeLayout) + backupFileSuffix
}

// ── Encode / decode ──────────────────────────────────────────────────────────

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// readBackup opens a backup (after checkBackupFile), decompresses at most
// limit bytes and checks the result is a relay snapshot. It returns the raw
// JSON, ready to become relay-state.json.
func readBackup(path string, limit int64) ([]byte, error) {
	if _, err := checkBackupFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The file checked above must be the file opened (no swap in between).
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || !ownedByMe(fi) || fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: %s changed while being opened", errUnsafe, path)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s is not a gzip file: %w", filepath.Base(path), err)
	}
	defer gz.Close()
	raw, err := io.ReadAll(io.LimitReader(gz, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", filepath.Base(path), err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s decompresses to more than %d bytes; refusing it (raise LANGSTROTH_RELAY_MAX_STORE_BYTES if the store is really this large)", filepath.Base(path), limit)
	}
	if _, err := parseSnapshot(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return raw, nil
}

// parseSnapshot checks raw is a relay snapshot. Its errors never quote the
// content (a snapshot holds token hashes and push tokens).
func parseSnapshot(raw []byte) (snapshot, error) {
	var snap snapshot
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&snap); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return snap, fmt.Errorf("not a relay snapshot (invalid JSON at byte %d)", se.Offset)
		}
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return snap, fmt.Errorf("not a relay snapshot (field %q has the wrong type)", te.Field)
		}
		return snap, errors.New("not a relay snapshot")
	}
	if dec.More() {
		return snap, errors.New("not a relay snapshot (trailing data)")
	}
	return snap, nil
}

// snapshotEmpty reports whether a snapshot holds nothing worth backing up.
func snapshotEmpty(s snapshot) bool {
	return len(s.Workspaces) == 0 && len(s.Directory) == 0 && len(s.Accounts) == 0 &&
		len(s.Users) == 0 && len(s.Tokens) == 0 && len(s.Contacts) == 0 && len(s.FriendReqs) == 0 &&
		len(s.FriendEdges) == 0 && (s.Tunnel == nil || tunnelSnapEmpty(s.Tunnel))
}

func tunnelSnapEmpty(t *tunnelSnap) bool {
	return len(t.Hosts) == 0 && len(t.Revocations) == 0 && len(t.PushTokens) == 0
}

// writeFileAtomic writes data to dir/name with mode 0600 through a fresh
// temp file (O_EXCL, so a planted symlink is never followed) and a rename.
func writeFileAtomic(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".tmp-"+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// ── Taking backups ───────────────────────────────────────────────────────────

// backupRunner takes backups of one data dir.
type backupRunner struct {
	cfg      backupConfig
	lastHash [sha256.Size]byte // of the newest backup's JSON; zero = unknown
	now      func() time.Time
}

func newBackupRunner(cfg backupConfig) *backupRunner {
	return &backupRunner{cfg: cfg, now: time.Now}
}

// backupOnce copies relay-state.json into a new backup and rotates. It skips
// an empty state (a wiped relay must not rotate good backups away) and a
// state identical to the newest backup (an idle relay adds nothing). It
// returns the new backup's name, or "" when it skipped.
func (r *backupRunner) backupOnce() (string, error) {
	raw, err := os.ReadFile(filepath.Join(r.cfg.dataDir, snapshotFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil // nothing flushed yet
	}
	if err != nil {
		return "", err
	}
	snap, err := parseSnapshot(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", snapshotFile, err)
	}
	if snapshotEmpty(snap) {
		return "", nil
	}
	if err := ensureBackupDir(r.cfg.dir); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	if r.lastHash == ([sha256.Size]byte{}) {
		r.lastHash = r.newestHash()
	}
	if sum == r.lastHash {
		return "", nil
	}
	data, err := gzipBytes(raw)
	if err != nil {
		return "", err
	}
	name := backupName(r.now())
	if err := writeFileAtomic(r.cfg.dir, name, data); err != nil {
		return "", err
	}
	r.lastHash = sum
	if err := rotateBackups(r.cfg.dir, r.cfg.keep); err != nil {
		slog.Warn("backup rotation failed", "dir", r.cfg.dir, "error", err)
	}
	slog.Info("backup written", "dir", r.cfg.dir, "name", name, "bytes", len(data))
	return name, nil
}

// newestHash is the SHA-256 of the newest backup's JSON, or zero.
func (r *backupRunner) newestHash() [sha256.Size]byte {
	list, err := listBackups(r.cfg.dir)
	if err != nil || len(list) == 0 {
		return [sha256.Size]byte{}
	}
	raw, err := readBackup(filepath.Join(r.cfg.dir, list[len(list)-1].Name), restoreLimit())
	if err != nil {
		return [sha256.Size]byte{}
	}
	return sha256.Sum256(raw)
}

// rotateBackups keeps the newest keep backups and deletes older ones. Only
// files with an exact backup name are ever deleted.
func rotateBackups(dir string, keep int) error {
	list, err := listBackups(dir)
	if err != nil || keep < 1 || len(list) <= keep {
		return err
	}
	for _, b := range list[:len(list)-keep] {
		if err := os.Remove(filepath.Join(dir, b.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// run takes a backup every interval until stop closes. Failures are logged;
// they never stop the relay. The shutdown backup is the caller's (after the
// final flush).
func (r *backupRunner) run(stop <-chan struct{}) {
	tick := time.NewTicker(r.cfg.interval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if _, err := r.backupOnce(); err != nil {
				slog.Error("backup failed", "dir", r.cfg.dir, "error", err)
			}
		}
	}
}

// ── Restoring ────────────────────────────────────────────────────────────────

// prepareDataDir runs before the memory store loads relay-state.json. An
// unreadable snapshot is moved aside (it used to be overwritten by the first
// flush); then, if there is no snapshot and restore is on, the newest backup
// that passes every check becomes relay-state.json. It never replaces a
// snapshot that parses.
func prepareDataDir(cfg backupConfig) {
	path := filepath.Join(cfg.dataDir, snapshotFile)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if _, perr := parseSnapshot(raw); perr == nil {
			return // a good snapshot: nothing to do
		} else {
			aside := path + ".unreadable-" + time.Now().UTC().Format(backupTimeLayout)
			if rerr := os.Rename(path, aside); rerr != nil {
				slog.Error("snapshot is unreadable and could not be moved aside", "path", path, "error", rerr)
				return
			}
			slog.Error("snapshot is unreadable; moved it aside", "path", path, "moved_to", aside, "error", perr.Error())
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return // can't read it (permissions): the store reports that
	}
	if !cfg.enabled || !cfg.restore {
		return
	}
	name, err := restoreNewest(cfg)
	switch {
	case err != nil:
		slog.Error("no backup could be restored; starting empty", "dir", cfg.dir, "error", err)
	case name != "":
		slog.Warn("restored state from backup", "dir", cfg.dir, "name", name,
			"note", "relay-state.json was missing or unreadable; set LANGSTROTH_RELAY_BACKUP_RESTORE=off to start empty instead")
	}
}

// restoreNewest writes the newest usable backup to relay-state.json, trying
// older ones when the newest fails a check. It returns the backup's name, or
// "" when there are none.
func restoreNewest(cfg backupConfig) (string, error) {
	if _, err := os.Lstat(cfg.dir); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err := checkBackupDir(cfg.dir); err != nil {
		return "", err
	}
	list, err := listBackups(cfg.dir)
	if err != nil {
		return "", err
	}
	var errs []error
	for i := len(list) - 1; i >= 0; i-- {
		raw, err := readBackup(filepath.Join(cfg.dir, list[i].Name), restoreLimit())
		if err != nil {
			slog.Warn("skipping backup", "name", list[i].Name, "error", err)
			errs = append(errs, err)
			continue
		}
		if err := installSnapshot(cfg.dataDir, raw); err != nil {
			return "", err
		}
		return list[i].Name, nil
	}
	if len(errs) > 0 {
		return "", fmt.Errorf("every backup failed a check: %w", errors.Join(errs...))
	}
	return "", nil
}

// installSnapshot makes raw the data dir's relay-state.json (mode 0600).
func installSnapshot(dataDir string, raw []byte) error {
	_, err := installSnapshotAside(dataDir, raw)
	return err
}

// installSnapshotAside is installSnapshot that first moves an existing
// relay-state.json aside to relay-state.json.before-restore-<time> and
// returns that path ("" when there was none).
func installSnapshotAside(dataDir string, raw []byte) (string, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, snapshotFile)
	aside := ""
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%w: %s is not a regular file", errUnsafe, path)
		}
		aside = path + ".before-restore-" + time.Now().UTC().Format(backupTimeLayout)
		if err := os.Rename(path, aside); err != nil {
			return "", err
		}
	}
	return aside, writeFileAtomic(dataDir, snapshotFile, raw)
}

// ── The restore command ──────────────────────────────────────────────────────

// cmdRestore is `langstroth-relay restore [--from NAME|PATH] [--json]`: with
// the relay stopped, make a backup the data dir's relay-state.json.
func cmdRestore(args []string, w io.Writer) error {
	var asJSON bool
	var from string
	args = splitFlagValues(args)
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--json":
			asJSON = true
		case "-o", "--output":
			if i+1 >= len(args) {
				return usageErrorf("%s needs json or text", a)
			}
			i++
			switch args[i] {
			case "json":
				asJSON = true
			case "text":
				asJSON = false
			default:
				return usageErrorf("%s must be json or text, not %q", a, args[i])
			}
		case "--from":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return usageErrorf("--from needs a backup name or path")
			}
			i++
			from = args[i]
		default:
			return usageErrorf("restore: unknown flag %s", a)
		}
	}
	fatalSettings := []error{removedEnvError(os.Environ()), secretFilesConfigError(os.Getenv)}
	for _, err := range fatalSettings {
		if err != nil {
			return &problem{Code: "config", Message: err.Error(), Fix: "fix the setting as the message says, then rerun", exit: exitFailed}
		}
	}
	cfg, err := backupConfigFromEnv()
	if err != nil {
		return &problem{Code: "config", Message: err.Error(), Fix: "see langstroth-relay help config (Backups)", exit: exitFailed}
	}
	if getenv("DATABASE_URL") != "" {
		return &problem{Code: "postgres", Message: "DATABASE_URL is set: built-in backups cover the memory store only",
			Why: "a Postgres relay's state is in the database", Fix: "restore the database with its own tooling (langstroth-relay help backup)", exit: exitFailed}
	}
	if cfg.dataDir == "" {
		return usageErrorf("LANGSTROTH_RELAY_DATA_DIR is not set: set it to the relay's data directory")
	}

	// Which backup. A name is looked up in the backup directory (and only an
	// exact backup name is accepted, so nothing can climb out of it); a path
	// is a file the operator chose, still held to every file check.
	var path string
	inDir := true
	switch {
	case from == "":
		list, err := listBackups(cfg.dir)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return &problem{Code: "not_found", Message: "no backups in " + cfg.dir,
				Fix: "check LANGSTROTH_RELAY_BACKUP_DIR, or pass --from PATH", exit: exitFailed}
		}
		path = filepath.Join(cfg.dir, list[len(list)-1].Name)
	case backupNameRe.MatchString(from):
		path = filepath.Join(cfg.dir, from)
	case strings.ContainsAny(from, "/"+string(filepath.Separator)):
		path, inDir = from, false
	default:
		return usageErrorf("--from %q is not a backup name (relay-backup-YYYYMMDDTHHMMSSZ.json.gz) or a path", from)
	}
	if inDir {
		if err := checkBackupDir(cfg.dir); err != nil {
			return &problem{Code: "unsafe", Message: err.Error(), Fix: "fix the directory's owner or mode", exit: exitFailed}
		}
	}

	// The relay must be stopped: it rewrites relay-state.json every 5 s and
	// would overwrite the restore with its in-memory state on shutdown.
	if fi, err := os.Stat(filepath.Join(cfg.dataDir, snapshotFile)); err == nil {
		if age := time.Since(fi.ModTime()); age < runningWindow {
			return &problem{Code: "running", Message: "the relay looks like it is running on " + cfg.dataDir,
				Why: fmt.Sprintf("%s changed %s ago (a running relay rewrites it every 5 s)", snapshotFile, age.Round(time.Second)),
				Fix: "stop the relay, wait 15 seconds, then rerun", exit: exitFailed}
		}
	}
	raw, err := readBackup(path, restoreLimit())
	if err != nil {
		p := &problem{Code: "backup", Message: err.Error(), Fix: "pick another backup with --from (ls " + cfg.dir + ")", exit: exitFailed}
		if errors.Is(err, errUnsafe) {
			p.Code, p.Fix = "unsafe", "fix the file's owner or mode (chown to the relay's user; chmod 600), or pick another backup"
		}
		return p
	}
	aside, err := installSnapshotAside(cfg.dataDir, raw)
	if err != nil {
		return err
	}
	rep := checkReport{Schema: checksSchema, Tool: "langstroth-relay", Command: "restore", OK: true,
		Next: []string{"start the relay", "langstroth-relay check --url URL"}}
	add := func(id, title, detail string) {
		rep.Checks = append(rep.Checks, checkResult{ID: id, Name: id, Title: title, Status: "ok", Detail: detail})
	}
	add("backup", "backup", fmt.Sprintf("%s: a valid snapshot (%d bytes)", path, len(raw)))
	add("relay-stopped", "relay stopped", snapshotFile+" not written in the last 15 s")
	moved := "there was no previous " + snapshotFile
	if aside != "" {
		moved = "the previous one is at " + aside
	}
	add("restored", "restored", filepath.Join(cfg.dataDir, snapshotFile)+" written; "+moved)
	if asJSON {
		return json.NewEncoder(w).Encode(rep)
	}
	for _, c := range rep.Checks {
		fmt.Fprintf(w, "%-5s %-17s %s\n", c.Status, c.Title, c.Detail)
	}
	fmt.Fprintln(w, "restored · next: start the relay")
	return nil
}

// ── check ────────────────────────────────────────────────────────────────────

// backupCheck is check's "backup" line.
func backupCheck(add func(name, status, detail, fix string)) {
	cfg, err := backupConfigFromEnv()
	switch {
	case err != nil:
		add("backup", "fail", err.Error(), "see `langstroth-relay help config` (Backups)")
		return
	case getenv("DATABASE_URL") != "":
		add("backup", "skip", "Postgres: back up the database with its own tooling", "see `langstroth-relay help backup`")
		return
	case cfg.dataDir == "":
		add("backup", "skip", "memory only: nothing is kept, so nothing to back up", "")
		return
	case !cfg.enabled:
		add("backup", "warn", "built-in backups are off (LANGSTROTH_RELAY_BACKUP=off)",
			"unset LANGSTROTH_RELAY_BACKUP, or copy relay-state.json yourself (langstroth-relay help backup)")
		return
	}
	if _, err := os.Lstat(cfg.dir); err == nil {
		if err := checkBackupDir(cfg.dir); err != nil {
			add("backup", "fail", err.Error(), "fix the directory's owner or mode, or set LANGSTROTH_RELAY_BACKUP_DIR")
			return
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		add("backup", "fail", err.Error(), "make LANGSTROTH_RELAY_BACKUP_DIR readable by the relay's user")
		return
	}
	list, err := listBackups(cfg.dir)
	if err != nil {
		add("backup", "fail", err.Error(), "make LANGSTROTH_RELAY_BACKUP_DIR readable by the relay's user")
		return
	}
	every := fmt.Sprintf("every %s, keeping %d, in %s", cfg.interval, cfg.keep, cfg.dir)
	if len(list) == 0 {
		add("backup", "ok", "no backups yet; "+every, "")
		return
	}
	newest := list[len(list)-1]
	age := time.Since(newest.Taken).Round(time.Minute)
	detail := fmt.Sprintf("%d backups, newest %s (%s ago); %s", len(list), newest.Name, age, every)
	add("backup", "ok", detail, "")
}
