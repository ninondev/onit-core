package main

// Self-host delivery commands for the synthetic first-run flow.
//
// The commands here are deliberately small and explicit: a new data directory
// must not exist, credentials live only in 0600 files, the seed goes through
// the public HTTP API, and backup/restore never write to the source directory.
// Nothing in this file reads a real vault, KeyStore, personal database or
// signing identity.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/version"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	deliveryFormat = "onit-selfhost/1"
	backupFormat   = "onit-backup/1"
	deliveryKind   = "synthetic"
	appVersion     = "onit-adviser/1"
)

type dataManifest struct {
	Format         string `json:"format"`
	Kind           string `json:"kind"`
	PersonalData   bool   `json:"personalData"`
	Seed           string `json:"seed"`
	InstallationID string `json:"installationId"`
	CreatedAt      string `json:"createdAt"`
	App            string `json:"app"`
}

type runtimeInfo struct {
	PID        int    `json:"pid"`
	Listen     string `json:"listen"`
	StartedAt  string `json:"startedAt"`
	App        string `json:"app"`
	InstanceID string `json:"instanceId"`
}

func defaultDBPath() string {
	cfg, _ := os.UserConfigDir()
	if cfg == "" {
		return ""
	}
	return filepath.Join(cfg, "onit-adviser", "memory.sqlite")
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := flags.String("dir", "", "new data directory (must not exist)")
	owner := flags.String("owner", "owner-a", "credential file name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *dir == "" {
		return fmt.Errorf("supply -dir with a new data directory")
	}
	if !validOwnerName(*owner) {
		return fmt.Errorf("owner name may use letters, digits, dot, dash and underscore")
	}
	if _, err := os.Lstat(*dir); err == nil {
		return fmt.Errorf("target already exists: %s; choose a new path", *dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(*dir, 0700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(*dir)
		}
	}()
	if err := os.Chmod(*dir, 0700); err != nil {
		return err
	}
	installationID, err := newInstallationID()
	if err != nil {
		return err
	}
	manifest := dataManifest{
		Format: deliveryFormat, Kind: deliveryKind, PersonalData: false,
		Seed: deliveryKind, InstallationID: installationID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), App: appVersion,
	}
	if err := writeJSONExclusive(filepath.Join(*dir, "manifest.json"), manifest, 0600); err != nil {
		return err
	}
	for _, name := range []string{"credentials", "seed"} {
		if err := os.Mkdir(filepath.Join(*dir, name), 0700); err != nil {
			return err
		}
	}
	db, err := OpenStore(filepath.Join(*dir, "memory.sqlite"))
	if err != nil {
		return err
	}
	token, createErr := db.CreateUser()
	closeErr := db.Close()
	if createErr != nil {
		return createErr
	}
	if closeErr != nil {
		return closeErr
	}
	tokenPath := filepath.Join(*dir, "credentials", *owner+".token")
	if err := writeSecretFile(tokenPath, token); err != nil {
		return err
	}
	cleanup = false
	fmt.Printf("initialized synthetic self-host data: %s\n", *dir)
	fmt.Printf("manifest: %s (kind=synthetic, personalData=false)\n", filepath.Join(*dir, "manifest.json"))
	fmt.Printf("owner credential: %s (0600, not printed)\n", tokenPath)
	fmt.Printf("next: %s serve -dir %s -listen 127.0.0.1:7341\n", os.Args[0], *dir)
	fmt.Printf("then: %s seed -dir %s -base http://127.0.0.1:7341 -token-file %s\n", os.Args[0], *dir, tokenPath)
	return nil
}

func newInstallationID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate installation id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// newRuntimeID names one serve process. runtime.json lives only in the
// directory that process serves and is excluded from backups, so a restored
// copy never inherits it.
func newRuntimeID() (string, error) {
	return newInstallationID()
}

// dataBindingDigest binds an installation to the real directory it serves.
// The digest is not reversible and is the only directory-derived value sent
// over the loopback API.
func dataBindingDigest(installationID, realDir string) string {
	sum := sha256.Sum256([]byte("onit-selfhost-binding/1\x00" + installationID + "\x00" + realDir))
	return hex.EncodeToString(sum[:])
}

func validOwnerName(name string) bool {
	if name == "" || len(name) > 40 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func runIssue(args []string) error {
	flags := flag.NewFlagSet("issue", flag.ContinueOnError)
	path := flags.String("db", defaultDBPath(), "private SQLite file")
	tokenFile := flags.String("token-file", "", "write the credential to this new 0600 file instead of stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *path == "" {
		return fmt.Errorf("supply -db when a user configuration directory is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0700); err != nil {
		return err
	}
	db, err := OpenStore(*path)
	if err != nil {
		return err
	}
	token, createErr := db.CreateUser()
	closeErr := db.Close()
	if createErr != nil {
		return createErr
	}
	if closeErr != nil {
		return closeErr
	}
	if *tokenFile != "" {
		if err := writeSecretFile(*tokenFile, token); err != nil {
			return err
		}
		fmt.Printf("wrote credential file: %s (0600, not printed)\n", *tokenFile)
		return nil
	}
	// The credential is returned once to the person provisioning this user.
	// All server diagnostics are on stderr and never contain credentials.
	fmt.Fprintln(os.Stdout, token)
	return nil
}

func validateDataDir(dir string) (dataManifest, error) {
	var manifest dataManifest
	info, err := os.Lstat(dir)
	if err != nil {
		return manifest, fmt.Errorf("data directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return manifest, fmt.Errorf("data directory is a symlink; refusing to follow it")
	}
	if !info.IsDir() {
		return manifest, fmt.Errorf("data path is not a directory")
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return manifest, fmt.Errorf("missing manifest.json; run init or restore a backup")
	}
	if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
		return manifest, fmt.Errorf("manifest.json is not a real file; refusing to follow it")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return manifest, fmt.Errorf("missing manifest.json; run init or restore a backup")
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, fmt.Errorf("manifest.json is not valid JSON")
	}
	if manifest.Format != deliveryFormat {
		return manifest, fmt.Errorf("unsupported manifest format %q", manifest.Format)
	}
	if manifest.Kind != deliveryKind {
		return manifest, fmt.Errorf("manifest kind is %q, want %q", manifest.Kind, deliveryKind)
	}
	if manifest.PersonalData {
		return manifest, fmt.Errorf("manifest marks personal data; refusing to use it as a synthetic self-host directory")
	}
	if manifest.Seed != deliveryKind {
		return manifest, fmt.Errorf("manifest seed is %q, want %q", manifest.Seed, deliveryKind)
	}
	if len(manifest.InstallationID) != 32 {
		return manifest, fmt.Errorf("manifest has no valid installationId; re-run init in a new directory")
	}
	if _, err := hex.DecodeString(manifest.InstallationID); err != nil {
		return manifest, fmt.Errorf("manifest installationId is not hexadecimal")
	}
	return manifest, nil
}

// realPath resolves an existing path to its real location. It is used for
// containment checks, never to follow a leaf that the caller must inspect.
func realPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// resolveNewPath resolves the parent of a path that must not exist yet.
func resolveNewPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// validateDataDirFiles checks the files a usable synthetic directory needs.
// deep adds a read-only SQLite quick_check; seed skips it because the server
// is already running and holds the database.
func validateDataDirFiles(dir string, deep bool) error {
	dbPath := filepath.Join(dir, "memory.sqlite")
	if deep {
		if err := checkDatabase(dbPath); err != nil {
			return err
		}
	} else if err := checkDatabaseFile(dbPath); err != nil {
		return err
	}
	credentials := filepath.Join(dir, "credentials")
	if err := checkCredentials(credentials); err != nil {
		return err
	}
	if err := checkCredentialsForDB(credentials, dbPath); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(dir, "seed"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("seed directory is missing or not a real directory")
	}
	if info.Mode().Perm() != 0700 {
		return fmt.Errorf("seed directory must be 0700 (found %04o)", info.Mode().Perm())
	}
	return nil
}

func checkCredentialsForDB(credentialsDir, dbPath string) error {
	if err := checkDatabaseFile(dbPath); err != nil {
		return err
	}
	entries, err := os.ReadDir(credentialsDir)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", readOnlyDSN(dbPath))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".token") {
			continue
		}
		token, err := readSecretFile(filepath.Join(credentialsDir, entry.Name()))
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(token))
		var id string
		err = db.QueryRow("SELECT id FROM users WHERE token_hash=?", sum[:]).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s does not match a user in memory.sqlite", entry.Name())
		}
		if err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("no .token credential files; run init or issue -token-file")
	}
	return nil
}

// dataDirForDB recognizes the compatibility form serve -db DIR/memory.sqlite.
// A directory that carries a manifest must pass the same checks as -dir; it is
// never treated as a plain database just because -dir was omitted.
func dataDirForDB(dbPath string) (string, bool, error) {
	absolute, err := filepath.Abs(dbPath)
	if err != nil {
		return "", false, err
	}
	parent := filepath.Dir(absolute)
	_, err = os.Lstat(filepath.Join(parent, "manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if filepath.Base(absolute) != "memory.sqlite" {
		return "", false, fmt.Errorf("database inside a data directory must be memory.sqlite; use -dir %s", parent)
	}
	return parent, true, nil
}

type dataLock struct {
	file *os.File
}

// lockPathForDB keeps the lock beside the data it protects. The lock is a
// separate file because macOS flock and SQLite's fcntl locks interfere on the
// same file; a sibling file leaves SQLite's own locking untouched.
func lockPathForDB(dbPath string) string {
	absolute, err := filepath.Abs(dbPath)
	if err != nil {
		absolute = dbPath
	}
	if filepath.Base(absolute) == "memory.sqlite" {
		return filepath.Join(filepath.Dir(absolute), "runtime.lock")
	}
	return absolute + ".lock"
}

// acquireLockFile takes a real advisory lock that cannot go stale: the kernel
// releases it when the process exits. create is false for read-only callers
// such as backup, which must not add a file to the source directory.
func acquireLockFile(path string, create bool) (*dataLock, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		if !create && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("another server holds the data lock; stop it before continuing")
		}
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	return &dataLock{file: file}, nil
}

func (l *dataLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}

func claimRuntime(path, addr, instanceID string) error {
	for attempt := 0; attempt < 2; attempt++ {
		info := runtimeInfo{PID: os.Getpid(), Listen: addr, StartedAt: time.Now().UTC().Format(time.RFC3339), App: appVersion, InstanceID: instanceID}
		raw, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			if _, err := f.Write(raw); err != nil {
				f.Close()
				os.Remove(path)
				return err
			}
			return f.Close()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		oldRaw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var old runtimeInfo
		if json.Unmarshal(oldRaw, &old) == nil && old.PID > 0 && pidAlive(old.PID) {
			return fmt.Errorf("already running (pid %d); stop it before starting another server", old.PID)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return fmt.Errorf("could not claim runtime file: %s", path)
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func runDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dir := flags.String("dir", "", "self-host data directory")
	listen := flags.String("listen", "", "optional loopback address to test")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *dir == "" {
		return fmt.Errorf("supply -dir with a self-host data directory")
	}
	ok, failed := 0, 0
	pass := func(name string) {
		ok++
		fmt.Printf("ok %s\n", name)
	}
	fail := func(name string, err error) {
		failed++
		fmt.Printf("FAIL %s: %v\n", name, err)
	}
	if _, err := validateDataDir(*dir); err != nil {
		fail("manifest", err)
	} else {
		pass("manifest")
	}
	if err := checkDatabase(filepath.Join(*dir, "memory.sqlite")); err != nil {
		fail("database", err)
	} else {
		pass("database")
	}
	if err := checkCredentials(filepath.Join(*dir, "credentials")); err != nil {
		fail("credentials", err)
	} else {
		pass("credentials")
	}
	if err := checkCredentialsForDB(filepath.Join(*dir, "credentials"), filepath.Join(*dir, "memory.sqlite")); err != nil {
		fail("credentials match database", err)
	} else {
		pass("credentials match database")
	}
	if info, err := os.Lstat(filepath.Join(*dir, "seed")); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		fail("seed directory", fmt.Errorf("missing or not a real directory"))
	} else {
		pass("seed directory")
	}
	if err := checkWritable(*dir); err != nil {
		fail("writable", err)
	} else {
		pass("writable")
	}
	if !version.IsValid(runtime.Version()) || version.Compare(runtime.Version(), "go1.26.0") < 0 {
		fail("go version", fmt.Errorf("built with %s; need Go 1.26 or newer", runtime.Version()))
	} else {
		pass("go version " + runtime.Version())
	}
	if *listen != "" {
		host, _, err := net.SplitHostPort(*listen)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			fail("listen", fmt.Errorf("use a numeric loopback address"))
		} else if listener, err := net.Listen("tcp", *listen); err != nil {
			fail("listen", fmt.Errorf("port is not available: %w", err))
		} else {
			listener.Close()
			pass("listen " + *listen)
		}
	}
	fmt.Printf("doctor: %d ok, %d failed\n", ok, failed)
	if failed > 0 {
		return fmt.Errorf("doctor found %d problem(s)", failed)
	}
	return nil
}

func checkDatabaseFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("missing memory.sqlite; run init or restore a backup")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("database is a symlink; refusing to follow it")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database is not a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("database must be 0600 (found %04o)", info.Mode().Perm())
	}
	return nil
}

func checkDatabase(path string) error {
	if err := checkDatabaseFile(path); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var result string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("quick_check: %s", result)
	}
	return nil
}

func checkCredentials(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("missing credentials directory")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("credentials is not a real directory")
	}
	if info.Mode().Perm() != 0700 {
		return fmt.Errorf("credentials directory must be 0700 (found %04o)", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".token") {
			continue
		}
		count++
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", entry.Name())
		}
		if info.Mode().Perm() != 0600 {
			return fmt.Errorf("%s must be 0600 (found %04o)", entry.Name(), info.Mode().Perm())
		}
		if info.Size() == 0 || info.Size() > 4096 {
			return fmt.Errorf("%s has an unexpected size", entry.Name())
		}
	}
	if count == 0 {
		return fmt.Errorf("no .token credential files; run init or issue -token-file")
	}
	return nil
}

func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return fmt.Errorf("directory is not writable: %w", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func readOnlyDSN(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	return (&url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro"}).String()
}

func writeJSONExclusive(path string, value any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeSecretFile(path, secret string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create credential file: %w", err)
	}
	if _, err := f.WriteString(secret + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readSecretFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("credential file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("credential file is a symlink; refusing to follow it")
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("credential file is not a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("credential file must be 0600 (found %04o)", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 24 || len(token) > 200 {
		return "", fmt.Errorf("credential file does not hold a valid token")
	}
	return token, nil
}

// ---- seed -----------------------------------------------------------------

const (
	seedSourceTitle  = "[合成] 课程退款与费用确认（synthetic）"
	seedCanaryTitle  = "[合成] 未选 canary（synthetic）"
	seedGoalTitle    = "[合成] 确认课程退款与最终费用（synthetic）"
	seedGoalStep     = "向课程方确认退款到账时间和最终费用"
	seedGoalUnit     = "次"
	seedRequest      = "核对：退款到账时间、最终费用。"
	seedTimezone     = "Asia/Shanghai"
	seedCanaryMarker = "synthetic-canary"
	seedSourceText   = "synthetic 合成资料，不是真实课程记录。\n退款到账时间尚未确定，必须询问课程方。\n最终费用必须等检测后确认。\n课程方说七个工作日内答复，但没有书面承诺。"
	seedCanaryText   = "synthetic-canary 这段文字不应出现在任何服务器准备成果里。"
)

type seedSource struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type seedPolicy struct {
	ClientKey string `json:"clientKey"`
	GoalID    string `json:"goalId"`
	Enabled   bool   `json:"enabled"`
	Version   int    `json:"version"`
}

type seedOutput struct {
	Revision int    `json:"revision"`
	Status   string `json:"status"`
	Date     string `json:"date"`
}

type seedSummary struct {
	OK           bool         `json:"ok"`
	Account      string       `json:"account,omitempty"`
	Sources      []seedSource `json:"sources"`
	GoalID       string       `json:"goalId,omitempty"`
	Policy       *seedPolicy  `json:"policy,omitempty"`
	Output       *seedOutput  `json:"output,omitempty"`
	CanaryLeak   bool         `json:"canaryLeak"`
	StoppedAfter string       `json:"stoppedAfter,omitempty"`
}

type seedFeed struct {
	Account   string `json:"account"`
	Goals     []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"goals"`
	Materials []struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"materials"`
	Preparations []seedPrep `json:"preparations"`
}

type seedPrep struct {
	ClientKey string `json:"clientKey"`
	GoalID    string `json:"goalId"`
	Enabled   bool   `json:"enabled"`
	Version   int    `json:"version"`
	Revision  int    `json:"revision"`
	Status    string `json:"status"`
	XML       string `json:"xml"`
	Date      string `json:"date"`
	Basis     []struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"basis"`
}

type seedAPI struct {
	base   string
	token  string
	client *http.Client
}

type selfhostIdentity struct {
	Format         string `json:"format"`
	Kind           string `json:"kind"`
	InstallationID string `json:"installationId"`
	App            string `json:"app"`
	// RuntimeID is generated per serve process and written to that data
	// directory's runtime.json. A restored copy keeps installationId and
	// credentials, so only this value distinguishes the process actually
	// serving the selected directory from a sibling copy.
	RuntimeID string `json:"runtimeId"`
	// DataBinding is a one-way digest of installationId and the real data
	// directory. It lets seed match the selected directory without sending
	// the internal path over HTTP.
	DataBinding string `json:"dataBinding"`
}

func runSeed(args []string) error {
	flags := flag.NewFlagSet("seed", flag.ContinueOnError)
	dir := flags.String("dir", "", "self-host data directory created by init")
	base := flags.String("base", "", "loopback API base, e.g. http://127.0.0.1:7341")
	tokenFile := flags.String("token-file", "", "0600 file holding the owner credential")
	clientKey := flags.String("client-key", "default", "preparation client key")
	enable := flags.Bool("enable", false, "explicitly enable the preparation and run one tick")
	stopAfter := flags.String("stop-after", "", "stop after source, goal or policy (interrupted retry test)")
	jsonOut := flags.Bool("json", false, "print only the JSON summary")
	timeout := flags.Duration("timeout", 20*time.Second, "HTTP timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *dir == "" || *base == "" || *tokenFile == "" {
		return fmt.Errorf("supply -dir, -base and -token-file")
	}
	if *stopAfter != "" && *stopAfter != "source" && *stopAfter != "goal" && *stopAfter != "policy" {
		return fmt.Errorf("stop-after must be source, goal or policy")
	}
	if !validPreparationClientKey(*clientKey) {
		return fmt.Errorf("client-key may use letters, digits, dot, dash and underscore")
	}
	manifest, err := validateDataDir(*dir)
	if err != nil {
		return err
	}
	if err := validateDataDirFiles(*dir, false); err != nil {
		return err
	}
	realDir, err := realPath(*dir)
	if err != nil {
		return err
	}
	realToken, err := resolveNewPath(*tokenFile)
	if err != nil {
		return fmt.Errorf("credential file: %w", err)
	}
	if filepath.Dir(realToken) != filepath.Join(realDir, "credentials") {
		return fmt.Errorf("credential file must be inside %s/credentials; this seed only writes to the selected data directory", *dir)
	}
	baseURL, err := loopbackBase(*base)
	if err != nil {
		return err
	}
	token, err := readSecretFile(realToken)
	if err != nil {
		return err
	}
	api := &seedAPI{base: baseURL, token: token, client: &http.Client{
		Timeout: *timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("redirect refused")
		},
	}}
	identity, err := api.selfhost()
	if err != nil {
		return err
	}
	// installationId and credentials survive backup/restore, so a restored
	// sibling can present both. Match the exact serve process recorded in the
	// selected directory before any write.
	runtime, runtimeErr := readRuntimeInfo(filepath.Join(realDir, "runtime.json"))
	if errors.Is(runtimeErr, os.ErrNotExist) {
		return fmt.Errorf("no running server recorded for %s; start adviser serve -dir %s", *dir, *dir)
	}
	if runtimeErr != nil {
		return runtimeErr
	}
	if runtime.InstanceID == "" || runtime.PID <= 0 || !pidAlive(runtime.PID) {
		return fmt.Errorf("no running server recorded for %s; start adviser serve -dir %s", *dir, *dir)
	}
	if identity.Kind != deliveryKind || identity.InstallationID == "" ||
		identity.InstallationID != manifest.InstallationID ||
		identity.RuntimeID != runtime.InstanceID ||
		identity.DataBinding != dataBindingDigest(manifest.InstallationID, realDir) {
		return fmt.Errorf("the server at %s is not the process serving %s; start adviser serve -dir %s", baseURL, *dir, *dir)
	}
	feed, err := api.feed()
	if err != nil {
		return err
	}
	summary := seedSummary{OK: true, Account: feed.Account, Sources: []seedSource{}}
	for _, spec := range []struct{ title, text string }{
		{seedSourceTitle, seedSourceText},
		{seedCanaryTitle, seedCanaryText},
	} {
		source, err := api.ensureSource(feed, spec.title, spec.text)
		if err != nil {
			return err
		}
		summary.Sources = append(summary.Sources, source)
	}
	if *stopAfter == "source" {
		summary.StoppedAfter = "source"
		printSeed(summary, *jsonOut)
		return nil
	}
	goalID, err := api.ensureGoal(feed)
	if err != nil {
		return err
	}
	summary.GoalID = goalID
	if *stopAfter == "goal" {
		summary.StoppedAfter = "goal"
		printSeed(summary, *jsonOut)
		return nil
	}
	policy, err := api.ensurePolicy(feed, goalID, summary.Sources[0].ID, *clientKey, *enable)
	if err != nil {
		return err
	}
	summary.Policy = policy
	if *stopAfter == "policy" {
		summary.StoppedAfter = "policy"
		printSeed(summary, *jsonOut)
		return nil
	}
	if *enable {
		ticked, err := api.tick()
		if err != nil {
			return err
		}
		prep := findPrep(ticked, *clientKey)
		if prep == nil {
			return fmt.Errorf("preparation %q missing after tick; read GET /v1/preparation", *clientKey)
		}
		if !prep.Enabled {
			return fmt.Errorf("preparation %q is not enabled after the explicit enable", *clientKey)
		}
		if prep.Status != "ready" {
			return fmt.Errorf("preparation %q status is %q, want ready; read GET /v1/preparation", *clientKey, prep.Status)
		}
		if prep.Revision < 1 {
			return fmt.Errorf("preparation %q has no revision after tick", *clientKey)
		}
		if !strings.Contains(prep.XML, "退款到账时间") || !strings.Contains(prep.XML, "最终费用") {
			return fmt.Errorf("preparation %q does not contain the selected source excerpts", *clientKey)
		}
		summary.CanaryLeak = strings.Contains(prep.XML, seedCanaryMarker)
		if summary.CanaryLeak {
			return fmt.Errorf("unselected canary leaked into the preparation output")
		}
		summary.Output = &seedOutput{Revision: prep.Revision, Status: prep.Status, Date: prep.Date}
	}
	printSeed(summary, *jsonOut)
	return nil
}

func loopbackBase(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("supply a plain http loopback base such as http://127.0.0.1:7341")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("the base must not include a path")
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return "http://" + u.Host, nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return "", fmt.Errorf("the base must be a numeric loopback address or localhost")
	}
	return "http://" + u.Host, nil
}

func (a *seedAPI) selfhost() (selfhostIdentity, error) {
	var identity selfhostIdentity
	err := a.call("GET", "/v1/selfhost", nil, &identity)
	return identity, err
}

func (a *seedAPI) feed() (seedFeed, error) {
	var feed seedFeed
	err := a.call("GET", "/v1/preparation", nil, &feed)
	return feed, err
}

func (a *seedAPI) tick() (seedFeed, error) {
	var feed seedFeed
	err := a.call("POST", "/v1/preparation/tick", map[string]any{}, &feed)
	return feed, err
}

func (a *seedAPI) proposals() ([]struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}, error) {
	var rows []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	err := a.call("GET", "/v1/proposals", nil, &rows)
	return rows, err
}

func (a *seedAPI) ensureSource(feed seedFeed, title, text string) (seedSource, error) {
	for _, material := range feed.Materials {
		if material.Kind == "source" && material.Title == title {
			return seedSource{ID: material.ID, Title: material.Title}, nil
		}
	}
	var created struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	body := map[string]string{"kind": "self", "origin": deliveryKind, "title": title, "text": text}
	if err := a.call("POST", "/v1/sources", body, &created); err != nil {
		return seedSource{}, err
	}
	if created.ID == "" {
		return seedSource{}, fmt.Errorf("source creation returned no id")
	}
	return seedSource{ID: created.ID, Title: created.Title}, nil
}

func (a *seedAPI) ensureGoal(feed seedFeed) (string, error) {
	for _, goal := range feed.Goals {
		if goal.Title == seedGoalTitle {
			return goal.ID, nil
		}
	}
	proposals, err := a.proposals()
	if err != nil {
		return "", err
	}
	for _, proposal := range proposals {
		if proposal.Title == seedGoalTitle {
			return a.accept(proposal.ID)
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := a.call("POST", "/v1/proposals", map[string]string{"title": seedGoalTitle}, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("proposal creation returned no id")
	}
	return a.accept(created.ID)
}

func (a *seedAPI) accept(proposalID string) (string, error) {
	var goal struct {
		ID string `json:"id"`
	}
	body := map[string]string{"title": seedGoalTitle, "step": seedGoalStep, "unit": seedGoalUnit}
	if err := a.call("POST", "/v1/proposals/"+url.PathEscape(proposalID)+"/accept", body, &goal); err != nil {
		return "", err
	}
	if goal.ID == "" {
		return "", fmt.Errorf("goal acceptance returned no id")
	}
	return goal.ID, nil
}

func (a *seedAPI) ensurePolicy(feed seedFeed, goalID, sourceID, clientKey string, enable bool) (*seedPolicy, error) {
	var existing *seedPrep
	for i := range feed.Preparations {
		if feed.Preparations[i].ClientKey == clientKey {
			existing = &feed.Preparations[i]
			break
		}
	}
	if existing != nil && !enable {
		return &seedPolicy{ClientKey: existing.ClientKey, GoalID: existing.GoalID, Enabled: existing.Enabled, Version: existing.Version}, nil
	}
	body := map[string]any{
		"goalId": goalID, "enabled": enable, "timezone": seedTimezone,
		"request": seedRequest,
		"selected": []map[string]string{{"kind": "source", "id": sourceID}},
	}
	var policy seedPolicy
	if err := a.call("PUT", "/v1/preparation/"+url.PathEscape(clientKey), body, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

func findPrep(feed seedFeed, clientKey string) *seedPrep {
	for i := range feed.Preparations {
		if feed.Preparations[i].ClientKey == clientKey {
			return &feed.Preparations[i]
		}
	}
	return nil
}

func (a *seedAPI) call(method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, a.base+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+a.token)
	request.Header.Set("Accept", "application/json")
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &problem)
		message := strings.TrimSpace(problem.Error)
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return fmt.Errorf("%s %s: HTTP %d %s", method, path, response.StatusCode, message)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: invalid JSON response", method, path)
		}
	}
	return nil
}

func printSeed(summary seedSummary, jsonOut bool) {
	if jsonOut {
		raw, _ := json.MarshalIndent(summary, "", "  ")
		fmt.Println(string(raw))
		return
	}
	fmt.Printf("account: %s\n", summary.Account)
	for _, source := range summary.Sources {
		fmt.Printf("source: %s %s\n", source.ID, source.Title)
	}
	if summary.GoalID != "" {
		fmt.Printf("goal: %s\n", summary.GoalID)
	}
	if summary.Policy != nil {
		fmt.Printf("policy: %s enabled=%t version=%d\n", summary.Policy.ClientKey, summary.Policy.Enabled, summary.Policy.Version)
	}
	if summary.Output != nil {
		fmt.Printf("output: revision=%d status=%s date=%s\n", summary.Output.Revision, summary.Output.Status, summary.Output.Date)
	}
	if summary.StoppedAfter != "" {
		fmt.Printf("stopped after: %s\n", summary.StoppedAfter)
	}
	fmt.Printf("canary leak: %t\n", summary.CanaryLeak)
}

// ---- backup and restore ---------------------------------------------------

type backupFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
}

type backupManifest struct {
	Format    string   `json:"format"`
	CreatedAt string   `json:"createdAt"`
	App       string   `json:"app"`
	Source    struct {
		Format       string `json:"format"`
		Kind         string `json:"kind"`
		PersonalData bool   `json:"personalData"`
	} `json:"source"`
	Dirs     []string     `json:"dirs"`
	Files    []backupFile `json:"files"`
	Excluded []string     `json:"excluded"`
	Notes    []string     `json:"notes"`
}

func runBackup(args []string) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	dir := flags.String("dir", "", "self-host data directory")
	out := flags.String("out", "", "new backup directory (must not exist)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *dir == "" || *out == "" {
		return fmt.Errorf("supply -dir and -out")
	}
	manifest, err := validateDataDir(*dir)
	if err != nil {
		return err
	}
	if err := validateDataDirFiles(*dir, true); err != nil {
		return err
	}
	if _, err := os.Lstat(*out); err == nil {
		return fmt.Errorf("backup target already exists: %s; choose a new path", *out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sourceReal, err := realPath(*dir)
	if err != nil {
		return err
	}
	outReal, err := resolveNewPath(*out)
	if err != nil {
		return fmt.Errorf("backup target: %w", err)
	}
	if pathContains(sourceReal, outReal) || pathContains(outReal, sourceReal) {
		return fmt.Errorf("backup target must be outside the source directory: %s", *out)
	}
	if err := refuseRunning(*dir); err != nil {
		return err
	}
	lock, err := acquireLockFile(lockPathForDB(filepath.Join(*dir, "memory.sqlite")), false)
	if err != nil {
		return fmt.Errorf("server may still be running: %w", err)
	}
	defer lock.Close()
	before, err := treeFingerprint(*dir)
	if err != nil {
		return err
	}
	files, dirs, err := collectTree(*dir)
	if err != nil {
		return err
	}
	if err := os.Mkdir(*out, 0700); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(*out)
		}
	}()
	backup := backupManifest{
		Format: backupFormat, CreatedAt: time.Now().UTC().Format(time.RFC3339), App: appVersion,
		Dirs: dirs, Files: []backupFile{}, Excluded: []string{"runtime.json", "runtime.lock"},
		Notes: []string{
			"Stop the server before backing up. SQLite WAL/SHM files present at that moment are copied as ordinary files.",
			"This backup contains only synthetic data. It does not contain a real vault, KeyStore, model key or mail credential.",
		},
	}
	backup.Source.Format = manifest.Format
	backup.Source.Kind = manifest.Kind
	backup.Source.PersonalData = manifest.PersonalData
	for _, rel := range files {
		source := filepath.Join(*dir, filepath.FromSlash(rel))
		target := filepath.Join(*out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		size, digest, err := copyBackupFile(source, target, info.Mode().Perm())
		if err != nil {
			return err
		}
		backup.Files = append(backup.Files, backupFile{Path: rel, Size: size, SHA256: digest, Mode: fmt.Sprintf("%04o", info.Mode().Perm())})
	}
	sort.Slice(backup.Files, func(i, j int) bool { return backup.Files[i].Path < backup.Files[j].Path })
	if err := writeJSONExclusive(filepath.Join(*out, "backup.json"), backup, 0600); err != nil {
		return err
	}
	after, err := treeFingerprint(*dir)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("source directory changed during backup; the backup was discarded")
	}
	cleanup = false
	fmt.Printf("backup: %s\n", *out)
	fmt.Printf("files: %d; directories: %d; runtime files excluded\n", len(backup.Files), len(backup.Dirs))
	fmt.Printf("note: this is synthetic data, not a personal vault or KeyStore backup.\n")
	return nil
}

func readRuntimeInfo(path string) (runtimeInfo, error) {
	var info runtimeInfo
	raw, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, fmt.Errorf("runtime.json is not valid JSON")
	}
	return info, nil
}

func refuseRunning(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var info runtimeInfo
	if json.Unmarshal(raw, &info) == nil && info.PID > 0 && pidAlive(info.PID) {
		return fmt.Errorf("server is running (pid %d); stop it before backup", info.PID)
	}
	return nil
}

func collectTree(root string) ([]string, []string, error) {
	var files, dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to back up symlink: %s", rel)
		}
		if entry.IsDir() {
			dirs = append(dirs, rel)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("refusing to back up non-regular file: %s", rel)
		}
		if rel == "runtime.json" || rel == "runtime.lock" {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	sort.Strings(files)
	sort.Strings(dirs)
	return files, dirs, err
}

// treeFingerprint covers every file and directory a backup would copy. It is
// computed before and after the copy so a backup can prove it left the source
// byte-for-byte unchanged.
func treeFingerprint(root string) (string, error) {
	files, dirs, err := collectTree(root)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, name := range dirs {
		fmt.Fprintf(hash, "d %s\n", name)
	}
	for _, name := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		digest, err := sha256File(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "f %s %d %04o %s\n", name, info.Size(), info.Mode().Perm(), digest)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyBackupFile(source, target string, mode os.FileMode) (int64, string, error) {
	in, err := os.Open(source)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, hash), in)
	if err != nil {
		out.Close()
		return 0, "", err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return 0, "", err
	}
	if err := out.Close(); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func runRestore(args []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	backupDir := flags.String("backup", "", "backup directory made by backup")
	dir := flags.String("dir", "", "new data directory to create")
	failAfter := flags.Int("fail-after", 0, "test hook: fail after N copied files (0 disables)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *backupDir == "" || *dir == "" {
		return fmt.Errorf("supply -backup and -dir")
	}
	if _, err := os.Lstat(*dir); err == nil {
		return fmt.Errorf("restore target already exists: %s; choose a new path", *dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	manifest, err := readBackupManifest(*backupDir)
	if err != nil {
		return err
	}
	if err := verifyBackup(*backupDir, manifest); err != nil {
		return err
	}
	backupReal, err := realPath(*backupDir)
	if err != nil {
		return err
	}
	targetReal, err := resolveNewPath(*dir)
	if err != nil {
		return fmt.Errorf("restore target: %w", err)
	}
	if pathContains(backupReal, targetReal) || pathContains(targetReal, backupReal) {
		return fmt.Errorf("restore target must be outside the backup directory: %s", *dir)
	}
	parent := filepath.Dir(filepath.Clean(*dir))
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		return fmt.Errorf("restore parent directory does not exist: %s", parent)
	}
	temp, err := os.MkdirTemp(parent, "."+filepath.Base(*dir)+".restore-")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(temp)
		}
	}()
	for _, rel := range manifest.Dirs {
		if err := os.MkdirAll(filepath.Join(temp, filepath.FromSlash(rel)), 0700); err != nil {
			return err
		}
	}
	for index, file := range manifest.Files {
		if *failAfter > 0 && index >= *failAfter {
			return fmt.Errorf("injected restore failure after %d files; target was not published", index)
		}
		mode, err := strconv.ParseUint(file.Mode, 8, 32)
		if err != nil {
			return fmt.Errorf("backup file mode is invalid: %s", file.Path)
		}
		target := filepath.Join(temp, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		size, digest, err := copyBackupFile(filepath.Join(*backupDir, filepath.FromSlash(file.Path)), target, os.FileMode(mode))
		if err != nil {
			return err
		}
		if size != file.Size || digest != file.SHA256 {
			return fmt.Errorf("restored file changed: %s", file.Path)
		}
	}
	if _, err := validateDataDir(temp); err != nil {
		return fmt.Errorf("restored data is not usable: %w", err)
	}
	if err := validateDataDirFiles(temp, true); err != nil {
		return fmt.Errorf("restored data is not usable: %w", err)
	}
	if err := os.Rename(temp, *dir); err != nil {
		return err
	}
	cleanup = false
	fmt.Printf("restored: %s\n", *dir)
	fmt.Printf("files: %d; database quick_check: ok\n", len(manifest.Files))
	fmt.Printf("next: %s serve -dir %s -listen 127.0.0.1:7341\n", os.Args[0], *dir)
	return nil
}

func readBackupManifest(dir string) (backupManifest, error) {
	var manifest backupManifest
	info, err := os.Lstat(dir)
	if err != nil {
		return manifest, fmt.Errorf("backup directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return manifest, fmt.Errorf("backup path is not a real directory")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "backup.json"))
	if err != nil {
		return manifest, fmt.Errorf("missing backup.json; this is not a backup made by adviser backup")
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, fmt.Errorf("backup.json is not valid JSON")
	}
	if manifest.Format != backupFormat {
		return manifest, fmt.Errorf("unsupported backup format %q", manifest.Format)
	}
	if manifest.Source.Format != deliveryFormat || manifest.Source.Kind != deliveryKind || manifest.Source.PersonalData {
		return manifest, fmt.Errorf("backup source is not a synthetic self-host directory")
	}
	if len(manifest.Files) == 0 {
		return manifest, fmt.Errorf("backup.json lists no files")
	}
	seen := map[string]bool{}
	for _, file := range manifest.Files {
		if !safeRelativePath(file.Path) || file.Path == "runtime.json" {
			return manifest, fmt.Errorf("backup contains an unsafe path: %q", file.Path)
		}
		if seen[file.Path] {
			return manifest, fmt.Errorf("backup lists a duplicate path: %s", file.Path)
		}
		seen[file.Path] = true
		mode, err := strconv.ParseUint(file.Mode, 8, 32)
		if err != nil || os.FileMode(mode).Perm()&0077 != 0 {
			return manifest, fmt.Errorf("backup file mode is not private: %s", file.Path)
		}
	}
	for _, dir := range manifest.Dirs {
		if !safeRelativePath(dir) {
			return manifest, fmt.Errorf("backup contains an unsafe directory: %q", dir)
		}
	}
	if !seen["manifest.json"] {
		return manifest, fmt.Errorf("backup does not contain manifest.json")
	}
	if !seen["memory.sqlite"] {
		return manifest, fmt.Errorf("backup does not contain memory.sqlite")
	}
	hasToken := false
	for path := range seen {
		if strings.HasPrefix(path, "credentials/") && strings.HasSuffix(path, ".token") {
			hasToken = true
			break
		}
	}
	if !hasToken {
		return manifest, fmt.Errorf("backup contains no credentials/*.token; it cannot be used after restore")
	}
	return manifest, nil
}

func safeRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean != path || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func verifyBackup(dir string, manifest backupManifest) error {
	listed := map[string]backupFile{}
	for _, file := range manifest.Files {
		listed[file.Path] = file
	}
	dirs := map[string]bool{}
	for _, name := range manifest.Dirs {
		dirs[name] = true
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("backup contains a symlink: %s", rel)
		}
		if entry.IsDir() {
			if !dirs[rel] {
				return fmt.Errorf("backup contains an unlisted directory: %s", rel)
			}
			return nil
		}
		if rel == "backup.json" {
			return nil
		}
		file, ok := listed[rel]
		if !ok {
			return fmt.Errorf("backup contains an unlisted file: %s", rel)
		}
		seen[rel] = true
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("backup file is not regular: %s", rel)
		}
		if info.Size() != file.Size {
			return fmt.Errorf("backup file size changed: %s", rel)
		}
		if fmt.Sprintf("%04o", info.Mode().Perm()) != file.Mode {
			return fmt.Errorf("backup file mode changed: %s", rel)
		}
		digest, err := sha256File(path)
		if err != nil {
			return err
		}
		if digest != file.SHA256 {
			return fmt.Errorf("backup file hash changed: %s", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, file := range manifest.Files {
		if !seen[file.Path] {
			return fmt.Errorf("backup is missing a listed file: %s", file.Path)
		}
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

