package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return usageError()
	}
	switch os.Args[1] {
	case "serve":
		return runServe(os.Args[2:])
	case "issue":
		return runIssue(os.Args[2:])
	case "android-bridge":
		return runBridge(os.Args[2:])
	case "init":
		return runInit(os.Args[2:])
	case "doctor":
		return runDoctor(os.Args[2:])
	case "seed":
		return runSeed(os.Args[2:])
	case "backup":
		return runBackup(os.Args[2:])
	case "restore":
		return runRestore(os.Args[2:])
	case "goal-review":
		return runGoalReview(os.Args[2:])
	case "goal-review-worker":
		return runGoalReviewWorker(os.Args[2:])
	default:
		return usageError()
	}
}

func usageError() error {
	return fmt.Errorf("usage: adviser serve|issue|android-bridge|init|doctor|seed|backup|restore|goal-review|goal-review-worker [flags]")
}

// runGoalReview is the headless entry point for the restricted local review.
// It uses the same dossier row, owner and revision CAS as the Android bridge;
// no browser or Node process is involved.
func runGoalReview(args []string) error {
	flags := flag.NewFlagSet("goal-review", flag.ContinueOnError)
	path := flags.String("db", defaultDBPath(), "private SQLite file")
	user := flags.String("user", "", "owner user id (default: the Android slot 1 owner)")
	goal := flags.String("goal", "", "goal id")
	action := flags.String("action", "show", "start|advance|run|restart|cancel|show")
	request := flags.String("request", "", "review request")
	jsonOut := flags.Bool("json", false, "print the full review JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *goal == "" {
		return fmt.Errorf("supply -goal")
	}
	if *path == "" {
		return fmt.Errorf("supply -db when a user configuration directory is unavailable")
	}
	switch *action {
	case grActionStart, grActionAdvance, grActionRun, grActionRestart, grActionCancel, grActionShow:
	default:
		return fmt.Errorf("unknown action %q", *action)
	}
	db, err := OpenStore(*path)
	if err != nil {
		return err
	}
	defer db.Close()
	review, err := db.GoalReview(*user, *goal, *action, *request)
	if err != nil {
		return err
	}
	if *jsonOut {
		fmt.Println(encode(review))
		return nil
	}
	fmt.Println("status:", str(review, "status"))
	if reason := str(review, "staleReason"); reason != "" {
		fmt.Println("reason:", reason)
	}
	if message := str(review, "error"); message != "" {
		fmt.Println("error:", message)
	}
	if result, _ := review["result"].(object); result != nil {
		fmt.Println("result:", str(result, "id"))
		fmt.Println(str(result, "body"))
	}
	return nil
}

// runGoalReviewWorker is the legal second OS process for the server-owned
// review. It does not take the serve lock: SQLite's immediate write
// transaction plus the work revision CAS are the cross-process protection.
// -steps 1 leaves a committed checkpoint that a later process can resume.
func runGoalReviewWorker(args []string) error {
	flags := flag.NewFlagSet("goal-review-worker", flag.ContinueOnError)
	path := flags.String("db", defaultDBPath(), "private SQLite file")
	user := flags.String("user", "", "owner user id")
	goal := flags.String("goal", "", "only this goal id (default: every followed goal of the owner)")
	steps := flags.Int("steps", grMaxTickSteps, "maximum committed transitions")
	nowRaw := flags.String("now", "", "test-only clock (RFC3339)")
	jsonOut := flags.Bool("json", false, "print full JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *user == "" {
		return fmt.Errorf("supply -user")
	}
	if *path == "" {
		return fmt.Errorf("supply -db when a user configuration directory is unavailable")
	}
	now := goalReviewNow()
	if strings.TrimSpace(*nowRaw) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*nowRaw))
		if err != nil {
			return fmt.Errorf("invalid -now")
		}
		now = t.UTC()
	}
	db, err := OpenStore(*path)
	if err != nil {
		return err
	}
	defer db.Close()
	if *goal != "" {
		made, err := db.AdvanceGoalReview(*user, *goal, *steps, now)
		if err != nil {
			return err
		}
		feed, err := db.GoalReviewFeed(*user, *goal, "")
		if err != nil {
			return err
		}
		if *jsonOut {
			fmt.Println(encode(object{"made": made, "feed": feed}))
			return nil
		}
		fmt.Println("made:", made)
		if work, _ := feed["work"].(object); work != nil {
			fmt.Println("status:", str(work, "status"))
			if reason := str(work, "blockedReason"); reason != "" {
				fmt.Println("blocked:", reason)
			}
			if result, _ := work["result"].(object); result != nil {
				fmt.Println("result:", str(result, "id"))
				fmt.Println(str(result, "body"))
			}
		}
		return nil
	}
	made, err := db.RunGoalReviewTick(*user, now)
	if err != nil {
		return err
	}
	feeds, err := db.GoalReviewFeeds(*user, "")
	if err != nil {
		return err
	}
	if *jsonOut {
		fmt.Println(encode(object{"made": made, "follows": feeds}))
		return nil
	}
	fmt.Println("made:", made)
	for _, feed := range feeds {
		work, _ := feed["work"].(object)
		status := ""
		if work != nil {
			status = str(work, "status")
		}
		fmt.Println(str(feed, "goalId"), status)
	}
	return nil
}

func runBridge(args []string) error {
	flags := flag.NewFlagSet("android-bridge", flag.ContinueOnError)
	path := flags.String("db", defaultDBPath(), "private SQLite file")
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
	defer db.Close()
	return runAndroidBridge(db, os.Stdin, os.Stdout)
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := flags.String("db", defaultDBPath(), "private SQLite file")
	dir := flags.String("dir", "", "self-host data directory created by init")
	addr := flags.String("listen", "127.0.0.1:7341", "loopback listener")
	endpoint := flags.String("endpoint", "", "optional model endpoint")
	model := flags.String("model", "", "optional model name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("listen on a numeric loopback address")
	}
	if *endpoint != "" && (!validModelEndpoint(*endpoint) || strings.Contains(*endpoint, "?")) {
		return fmt.Errorf("provide an HTTPS or loopback model endpoint without credentials or query parameters")
	}
	if (*endpoint == "") != (*model == "") {
		return fmt.Errorf("supply both -endpoint and -model")
	}
	dbPath := *path
	dataDir := *dir
	runtimePath := ""
	var manifest dataManifest
	haveManifest := false
	if dataDir != "" {
		if flagWasSet(flags, "db") {
			return fmt.Errorf("use either -dir or -db, not both")
		}
		manifest, err = validateDataDir(dataDir)
		if err != nil {
			return err
		}
		if err := validateDataDirFiles(dataDir, true); err != nil {
			return err
		}
		dbPath = filepath.Join(dataDir, "memory.sqlite")
		haveManifest = true
	} else {
		inferred, isDataDir, err := dataDirForDB(dbPath)
		if err != nil {
			return err
		}
		if isDataDir {
			manifest, err = validateDataDir(inferred)
			if err != nil {
				return err
			}
			if err := validateDataDirFiles(inferred, true); err != nil {
				return err
			}
			dataDir = inferred
			haveManifest = true
		}
	}
	runtimeID := ""
	if dataDir != "" {
		runtimeID, err = newRuntimeID()
		if err != nil {
			return err
		}
		runtimePath = filepath.Join(dataDir, "runtime.json")
		if err := claimRuntime(runtimePath, *addr, runtimeID); err != nil {
			return err
		}
		defer os.Remove(runtimePath)
	}
	if dbPath == "" {
		return fmt.Errorf("supply -db when a user configuration directory is unavailable")
	}
	// Plain -db mode is the compatibility path for an existing database. A
	// missing file must not silently become a new empty database: that would
	// report a successful upgrade while the old facts are gone.
	if !haveManifest {
		info, err := os.Stat(dbPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("database file does not exist: %s (run adviser init -dir ... or adviser issue -db ... first)", dbPath)
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("database path is not a regular file: %s", dbPath)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return err
	}
	lock, err := acquireLockFile(lockPathForDB(dbPath), true)
	if err != nil {
		return err
	}
	defer lock.Close()
	db, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	// agent 调度：默认每小时一轮。ADVISER_TICK_INTERVAL 是隔离验收用的
	// 测试专用短间隔，产品不设置它。失败只记 stderr，不中断服务。
	interval := time.Hour
	if raw := strings.TrimSpace(os.Getenv("ADVISER_TICK_INTERVAL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= time.Second && d <= 24*time.Hour {
			interval = d
		}
	}
	go func() {
		for {
			now := goalReviewNow()
			if _, err := db.RunAgentTick(now); err != nil {
				fmt.Fprintln(os.Stderr, "agent tick:", err)
			}
			// 主动准备按 owner 逐个推进；只读服务端来源，不碰 Android slot=1。
			if _, err := db.RunPreparationTickAll(now); err != nil {
				fmt.Fprintln(os.Stderr, "preparation tick:", err)
			}
			// 持续目标复盘只推进已明确开启的 owner+goal；旧 nudge 在同一
			// 轮里已经让位给同一批纳管目标。
			if _, err := db.RunGoalReviewTickAll(now); err != nil {
				fmt.Fprintln(os.Stderr, "goal review tick:", err)
			}
			time.Sleep(interval)
		}
	}()
	if err = db.completeStoredReadings(""); err != nil {
		return err
	}
	identity := selfhostIdentity{}
	if haveManifest {
		realDir, err := realPath(dataDir)
		if err != nil {
			return err
		}
		identity = selfhostIdentity{
			Format: manifest.Format, Kind: manifest.Kind, InstallationID: manifest.InstallationID, App: manifest.App,
			RuntimeID: runtimeID, DataBinding: dataBindingDigest(manifest.InstallationID, realDir),
		}
	}
	server := &http.Server{Addr: *addr, Handler: newService(db, Model{Endpoint: *endpoint, Name: *model}, identity), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 25 * time.Second, WriteTimeout: 100 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		grace, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(grace)
		close(done)
	}()
	if dataDir != "" {
		fmt.Fprintln(os.Stderr, "synthetic self-host data:", dataDir, "(not personal data)")
	}
	fmt.Fprintln(os.Stderr, "listening on", listener.Addr())
	if err = server.Serve(listener); err != http.ErrServerClosed {
		return err
	}
	<-done
	return nil
}
