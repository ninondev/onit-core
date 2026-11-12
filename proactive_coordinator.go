package main

// Shared persistent coordination for local preparation.
//
// Every entry point (the open page, the Android JobService, boot/package
// recovery) goes through one tick: read the persisted policy and the dossier
// revision/snapshot, run the unconditional maintenance pass, generate only
// when the policy allows it, then commit with CAS. A pause therefore survives
// process death and is not just a cancelled job or a hidden button.
//
// The settings file is the Android app's own settings JSON. Missing
// "proactive" keeps the old foreground behaviour; missing "backgroundPrepare"
// means no background generation, so an upgrade never silently expands what
// the app does. A corrupt settings file denies generation but still allows the
// maintenance pass to remove withdrawn or corrected derived text.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	androidSettingsFile = "openquest.advisor.settings.v2.json"
	proactiveStatusFile = "proactive-status.json"
	proactiveLockFile   = "proactive-policy.lock"
	proactiveMaxTries   = 3
)

// withProactivePolicyLock serializes the final policy read and the CAS commit
// with the native settings writer. The writer takes the same fcntl lock before
// replacing the settings file, so a pause cannot land in the window between
// "the policy still allows generation" and "the generated state is durable".
// The lock is released by the kernel if this process exits.
func withProactivePolicyLock(dbPath string, fn func() error) error {
	if dbPath == "" || dbPath == ":memory:" {
		return fn()
	}
	path := filepath.Join(filepath.Dir(dbPath), proactiveLockFile)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EACCES) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("proactive policy lock timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer func() {
		unlock := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0}
		_ = unix.FcntlFlock(file.Fd(), unix.F_SETLK, &unlock)
	}()
	return fn()
}

type proactivePolicy struct {
	Proactive  bool   `json:"proactive"`
	Background bool   `json:"backgroundPrepare"`
	State      string `json:"state"` // ok, missing, corrupt
	Hash       string `json:"hash"`
}

func (p proactivePolicy) allows(source string) bool {
	if p.State == "corrupt" {
		return false
	}
	switch source {
	case "maintenance":
		return false
	case "background":
		return p.Proactive && p.Background
	default:
		return p.Proactive
	}
}

// readProactivePolicy treats a present but non-boolean switch as corruption,
// not as the old default. A hand-edited or truncated file must not silently
// turn background work back on.
func readProactivePolicy(dbPath string) proactivePolicy {
	policy := proactivePolicy{Proactive: true, Background: false, State: "missing"}
	if dbPath == "" || dbPath == ":memory:" {
		return policy
	}
	path := filepath.Join(filepath.Dir(dbPath), androidSettingsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return policy
	}
	if err != nil {
		return proactivePolicy{State: "corrupt"}
	}
	sum := sha256.Sum256(data)
	policy.Hash = hex.EncodeToString(sum[:])
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return proactivePolicy{State: "corrupt", Hash: policy.Hash}
	}
	policy.State = "ok"
	if value, present := raw["proactive"]; present {
		flag, ok := value.(bool)
		if !ok {
			return proactivePolicy{State: "corrupt", Hash: policy.Hash}
		}
		policy.Proactive = flag
	}
	if value, present := raw["backgroundPrepare"]; present {
		flag, ok := value.(bool)
		if !ok {
			return proactivePolicy{State: "corrupt", Hash: policy.Hash}
		}
		policy.Background = flag
	}
	return policy
}

type proactiveStatus struct {
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
	LastSuccessAt string `json:"lastSuccessAt,omitempty"`
	LastResult    string `json:"lastResult,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	LastSource    string `json:"lastSource,omitempty"`
	LastPolicy    string `json:"lastPolicy,omitempty"`
	LastRevision  int    `json:"lastRevision,omitempty"`
	LastMade      int    `json:"lastMade,omitempty"`
}

func readProactiveStatus(dbPath string) proactiveStatus {
	var status proactiveStatus
	if dbPath == "" || dbPath == ":memory:" {
		return status
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), proactiveStatusFile))
	if err != nil {
		return status
	}
	_ = json.Unmarshal(data, &status)
	return status
}

// The status file is diagnostic only. It is never read as a dedupe cursor or
// as proof that a card exists; a failed write is ignored.
func writeProactiveStatus(dbPath string, status proactiveStatus) {
	if dbPath == "" || dbPath == ":memory:" {
		return
	}
	path := filepath.Join(filepath.Dir(dbPath), proactiveStatusFile)
	data, err := json.Marshal(status)
	if err != nil {
		return
	}
	pending := path + ".new"
	if err := os.WriteFile(pending, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(pending, path)
}

func proactiveNow() string {
	return proactiveClock().UTC().Format(time.RFC3339)
}

func recordProactiveResult(dbPath, source string, policy proactivePolicy, revision, made int, result string, failure error) {
	status := readProactiveStatus(dbPath)
	status.LastAttemptAt = proactiveNow()
	status.LastResult = result
	status.LastSource = source
	status.LastPolicy = policy.State
	status.LastRevision = revision
	status.LastMade = made
	if failure != nil {
		status.LastError = failure.Error()
	} else {
		status.LastError = ""
		status.LastSuccessAt = status.LastAttemptAt
	}
	writeProactiveStatus(dbPath, status)
}

// Test seams. Tests in this package run sequentially; a test that installs a
// hook must clear it with t.Cleanup.
var (
	proactiveClock    = time.Now
	proactiveTickHook func(stage string) error
	proactiveHookMu   sync.Mutex
)

func proactiveHook(stage string) error {
	proactiveHookMu.Lock()
	hook := proactiveTickHook
	proactiveHookMu.Unlock()
	if hook == nil {
		return nil
	}
	return hook(stage)
}

func cloneObject(in object) object {
	data, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var out object
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// runProactiveTick is the one coordination entry. It returns the same shape as
// the old tick reply plus the policy and status, so the page and the
// JobService can both show an honest state.
func (s *Store) runProactiveTick(source string) (object, error) {
	if source == "" {
		source = "foreground"
	}
	if source != "foreground" && source != "background" && source != "maintenance" {
		return nil, ErrInvalid
	}
	var lastErr error
	for attempt := 1; attempt <= proactiveMaxTries; attempt++ {
		current, err := s.androidState(nil, nil, "")
		if err != nil {
			return nil, err
		}
		revision := int(number(current["revision"]))
		snapshot := str(current, "snapshot")
		state, _ := current["state"].(object)
		policy := readProactivePolicy(s.dbPath)
		if state == nil {
			recordProactiveResult(s.dbPath, source, policy, revision, 0, "empty", nil)
			// No dossier yet: do not invent a revision or snapshot. The page
			// treats an empty reply as first run, not as a writable base.
			return object{
				"ok": true, "made": 0, "changed": false, "artifacts": []any{},
				"policy": policy, "status": readProactiveStatus(s.dbPath),
				"source": source, "generated": false, "attempts": attempt,
			}, nil
		}
		if err := proactiveHook("after-read"); err != nil {
			recordProactiveResult(s.dbPath, source, policy, revision, 0, "error", err)
			return nil, err
		}

		// Maintenance always runs. It is what removes withdrawn or corrected
		// derived text while generation is paused.
		maintenance := state
		_, maintenanceChanged, err := RunProactiveWithGeneration(maintenance, proactiveClock(), false)
		if err != nil {
			recordProactiveResult(s.dbPath, source, policy, revision, 0, "error", err)
			return nil, err
		}
		if err := proactiveHook("after-maintenance"); err != nil {
			recordProactiveResult(s.dbPath, source, policy, revision, 0, "error", err)
			return nil, err
		}

		commitState := maintenance
		made := []object{}
		changed := maintenanceChanged
		generated := false
		var generatedState object
		var generatedMade []object
		generatedChanged := false
		// Re-read after maintenance: a pause that lands while the maintenance
		// pass runs must not be followed by generation.
		latest := readProactivePolicy(s.dbPath)
		if policy.allows(source) && latest.allows(source) {
			clone := cloneObject(maintenance)
			if clone == nil {
				err := errors.New("state could not be copied")
				recordProactiveResult(s.dbPath, source, latest, revision, 0, "error", err)
				return nil, err
			}
			genMade, genChanged, err := RunProactiveWithGeneration(clone, proactiveClock(), true)
			if err != nil {
				recordProactiveResult(s.dbPath, source, latest, revision, 0, "error", err)
				return nil, err
			}
			if err := proactiveHook("after-generate"); err != nil {
				recordProactiveResult(s.dbPath, source, latest, revision, 0, "error", err)
				return nil, err
			}
			generatedState = clone
			generatedMade = genMade
			generatedChanged = genChanged
		}
		policy = latest

		if maintenanceChanged || generatedChanged {
			if err := proactiveHook("before-commit"); err != nil {
				recordProactiveResult(s.dbPath, source, policy, revision, 0, "error", err)
				return nil, err
			}
		}
		// The final policy read and the CAS share one cross-process lock with
		// the native settings writer. A pause that lands before this point is
		// seen here; a pause that lands while we hold the lock waits until the
		// commit is durable. This is a real boundary, not another read with the
		// same window.
		err = withProactivePolicyLock(s.dbPath, func() error {
			atCommit := readProactivePolicy(s.dbPath)
			policy = atCommit
			if generatedState != nil && atCommit.allows(source) {
				commitState = generatedState
				made = generatedMade
				changed = maintenanceChanged || generatedChanged
				generated = true
			} else {
				commitState = maintenance
				made = []object{}
				changed = maintenanceChanged
				generated = false
			}
			// Always use the CAS as a freshness check, even when this pass
			// changed nothing. A no-op computed from a stale read must not be
			// reported as success: a withdrawal or edit that landed after the
			// read has to force a retry. When the state is current this is an
			// exact retry and writes nothing.
			_, err := s.androidState(&revision, commitState, snapshot)
			return err
		})
		if err != nil {
			if errors.Is(err, ErrConflict) {
				lastErr = err
				continue
			}
			recordProactiveResult(s.dbPath, source, policy, revision, 0, "error", err)
			return nil, err
		}
		if changed {
			if err := proactiveHook("after-commit"); err != nil {
				// The card is durable; only the reply was lost. Do not mark the
				// attempt successful, and do not roll the commit back.
				recordProactiveResult(s.dbPath, source, policy, revision, len(made), "error", err)
				return nil, err
			}
		}
		updated, err := s.androidState(nil, nil, "")
		if err != nil {
			recordProactiveResult(s.dbPath, source, policy, revision, len(made), "error", err)
			return nil, err
		}
		recordProactiveResult(s.dbPath, source, policy, int(number(updated["revision"])), len(made), "ok", nil)
		return object{
			"ok": true, "made": len(made), "changed": changed, "artifacts": toAnyObjects(made),
			"revision": updated["revision"], "snapshot": updated["snapshot"], "state": updated["state"],
			"policy": policy, "status": readProactiveStatus(s.dbPath),
			"source": source, "generated": generated, "attempts": attempt,
		}, nil
	}
	if lastErr == nil {
		lastErr = ErrConflict
	}
	recordProactiveResult(s.dbPath, source, readProactivePolicy(s.dbPath), 0, 0, "error", lastErr)
	return nil, lastErr
}

func proactivePolicyReply(dbPath string) object {
	return object{
		"ok":     true,
		"policy": readProactivePolicy(dbPath),
		"status": readProactiveStatus(dbPath),
	}
}
