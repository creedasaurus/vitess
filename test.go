///bin/true; exec /usr/bin/env go run "$0" "$@"

// Copyright 2015, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
test.go is a "Go script" for running Vitess tests. It runs each test in its own
Docker container for hermeticity and (potentially) parallelism. If a test fails,
this script will save the output in _test/ and continue with other tests.

Before using it, you should have Docker 1.5+ installed, and have your user in
the group that lets you run the docker command without sudo. The first time you
run against a given flavor, it may take some time for the corresponding
bootstrap image (vitess/bootstrap:<flavor>) to be downloaded.

It is meant to be run from the Vitess root, like so:
  ~/src/github.com/youtube/vitess$ go run test.go [args]

For a list of options, run:
  $ go run test.go --help
*/
package main

// This Go script shouldn't rely on any packages that aren't in the standard
// library, since that would require the user to bootstrap before running it.
import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var usage = `Usage of test.go:

go run test.go [options] [test_name ...]

If one or more test names are provided, run only those tests.
Otherwise, run all tests in test/config.json.
`

// Flags
var (
	flavor   = flag.String("flavor", "mariadb", "bootstrap flavor to run against")
	runCount = flag.Int("runs", 1, "run each test this many times")
	retryMax = flag.Int("retry", 3, "max number of retries, to detect flaky tests")
	logPass  = flag.Bool("log-pass", false, "log test output even if it passes")
	timeout  = flag.Duration("timeout", 10*time.Minute, "timeout for each test")
	pull     = flag.Bool("pull", true, "re-pull the bootstrap image, in case it's been updated")
	docker   = flag.Bool("docker", true, "run tests with Docker")
	shard    = flag.Int("shard", -1, "if >=0, run the tests whose Shard matches")
	reshard  = flag.Int("reshard", 0, "if >0, check the stats and group tests into similarly-sized bins by average run time")
	keepData = flag.Bool("keep-data", false, "don't delete the per-test VTDATAROOT subfolders")
	printLog = flag.Bool("print-log", false, "print the log of each failed test (or all tests if -log-pass) to the console")
	follow   = flag.Bool("follow", false, "print test output as it runs, instead of waiting to see if it passes or fails")

	remoteStats = flag.String("remote-stats", "", "url to send remote stats")

	extraArgs = flag.String("extra-args", "", "extra args to pass to each test")
)

var vtDataRoot = os.Getenv("VTDATAROOT")

const (
	statsFileName  = "test/stats.json"
	configFileName = "test/config.json"
)

// Config is the overall object serialized in test/config.json.
type Config struct {
	Tests map[string]*Test
}

// Test is an entry from the test/config.json file.
type Test struct {
	File, Args, Command string

	// Manual means it won't be run unless explicitly specified.
	Manual bool

	// Shard is used to split tests among workers.
	Shard int

	name     string
	cmd      *exec.Cmd
	runIndex int

	pass, fail int
}

// run executes a single try.
// dir is the location of the vitess repo to use.
// dataDir is the VTDATAROOT to use for this run.
// returns the combined stdout+stderr and error.
func (t *Test) run(dir, dataDir string) ([]byte, error) {
	testCmd := t.Command
	if testCmd == "" {
		testCmd = fmt.Sprintf("make build && test/%s -v --skip-build --keep-logs %s", t.File, t.Args)
		if *docker {
			// Teardown is unnecessary since Docker kills everything.
			testCmd += " --skip-teardown"
		}
		if *extraArgs != "" {
			testCmd += " " + *extraArgs
		}
	}

	if *docker {
		t.cmd = exec.Command(path.Join(dir, "docker/test/run.sh"), *flavor, testCmd)
	} else {
		t.cmd = exec.Command("bash", "-c", testCmd)
	}
	t.cmd.Dir = dir

	// Put everything in a unique dir, so we can copy and/or safely delete it.
	t.cmd.Env = updateEnv(os.Environ(), map[string]string{
		"VTDATAROOT": dataDir,
	})

	// Stop the test if it takes too long.
	done := make(chan struct{})
	timer := time.NewTimer(*timeout)
	defer timer.Stop()
	go func() {
		select {
		case <-done:
		case <-timer.C:
			t.logf("timeout exceeded")
			if t.cmd.Process != nil {
				t.cmd.Process.Signal(syscall.SIGTERM)
			}
		}
	}()

	defer close(done)

	// Capture test output.
	buf := &bytes.Buffer{}
	t.cmd.Stdout = buf
	if *follow {
		t.cmd.Stdout = io.MultiWriter(t.cmd.Stdout, os.Stdout)
	}
	t.cmd.Stderr = t.cmd.Stdout

	// Run the test.
	err := t.cmd.Run()
	if err == nil {
		t.pass++
	} else {
		t.fail++
	}
	return buf.Bytes(), err
}

// stop will terminate the test if it's running.
// If the test is not running, it's a no-op.
func (t *Test) stop() {
	if cmd := t.cmd; cmd != nil {
		if proc := cmd.Process; proc != nil {
			proc.Signal(syscall.SIGTERM)
		}
	}
}

func (t *Test) logf(format string, v ...interface{}) {
	if *runCount > 1 {
		log.Printf("%v[%v/%v]: %v", t.name, t.runIndex+1, *runCount, fmt.Sprintf(format, v...))
	} else {
		log.Printf("%v: %v", t.name, fmt.Sprintf(format, v...))
	}
}

func main() {
	flag.Usage = func() {
		os.Stderr.WriteString(usage)
		os.Stderr.WriteString("\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	startTime := time.Now()

	// Make output directory.
	outDir := path.Join("_test", fmt.Sprintf("%v.%v.%v", *flavor, startTime.Format("20060102-150405"), os.Getpid()))
	if err := os.MkdirAll(outDir, os.FileMode(0755)); err != nil {
		log.Fatalf("Can't create output directory: %v", err)
	}
	logFile, err := os.OpenFile(path.Join(outDir, "test.log"), os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		log.Fatalf("Can't create log file: %v", err)
	}
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	log.Printf("Output directory: %v", outDir)

	// Get test configs.
	configData, err := ioutil.ReadFile(configFileName)
	if err != nil {
		log.Fatalf("Can't read config file: %v", err)
	}
	var config Config
	if err := json.Unmarshal(configData, &config); err != nil {
		log.Fatalf("Can't parse config file: %v", err)
	}

	// Resharding.
	if *reshard > 0 {
		if err := reshardTests(&config, *reshard); err != nil {
			log.Fatalf("resharding error: %v", err)
		}
		log.Printf("Saving updated config...")
		data, err := json.MarshalIndent(config, "", "\t")
		if err != nil {
			log.Fatalf("can't save new config: %v", err)
		}
		if err := ioutil.WriteFile(configFileName, data, 0644); err != nil {
			log.Fatalf("can't write new config: %v", err)
		}
		return
	}

	if *docker {
		log.Printf("Bootstrap flavor: %v", *flavor)

		// Re-pull image.
		if *docker && *pull {
			image := "vitess/bootstrap:" + *flavor
			pullTime := time.Now()
			log.Printf("Pulling %v...", image)
			cmd := exec.Command("docker", "pull", image)
			if out, err := cmd.CombinedOutput(); err != nil {
				log.Fatalf("Can't pull image: %v\n%s", err, out)
			}
			log.Printf("Image pulled in %v", time.Since(pullTime))
		}
	} else {
		if vtDataRoot == "" {
			log.Fatalf("VTDATAROOT env var must be set in -docker=false mode. Make sure to source dev.env.")
		}
	}

	// Pick the tests to run.
	tests := selectedTests(&config)

	// Duplicate tests.
	if *runCount > 1 {
		var dup []*Test
		for _, t := range tests {
			for i := 0; i < *runCount; i++ {
				// Make a copy, since they're pointers.
				test := *t
				test.runIndex = i
				dup = append(dup, &test)
			}
		}
		tests = dup
	}

	vtTop := "."
	tmpDir := ""
	if *docker {
		// Copy working repo to tmpDir.
		// This doesn't work outside Docker since it messes up GOROOT.
		tmpDir, err = ioutil.TempDir(os.TempDir(), "vt_")
		if err != nil {
			log.Fatalf("Can't create temp dir in %v", os.TempDir())
		}
		log.Printf("Copying working repo to temp dir %v", tmpDir)
		if out, err := exec.Command("cp", "-R", ".", tmpDir).CombinedOutput(); err != nil {
			log.Fatalf("Can't copy working repo to temp dir %v: %v: %s", tmpDir, err, out)
		}
		// The temp copy needs permissive access so the Docker user can read it.
		if out, err := exec.Command("chmod", "-R", "go=u", tmpDir).CombinedOutput(); err != nil {
			log.Printf("Can't set permissions on temp dir %v: %v: %s", tmpDir, err, out)
		}
		vtTop = tmpDir
	}

	// Keep stats.
	failed := 0
	passed := 0
	flaky := 0

	// Listen for signals.
	sigchan := make(chan os.Signal)
	signal.Notify(sigchan, syscall.SIGINT, syscall.SIGTERM)

	// Run tests.
	stop := make(chan struct{}) // Close this to tell the loop to stop.
	done := make(chan struct{}) // The loop closes this when it has stopped.
	go func() {
		defer func() {
			signal.Stop(sigchan)
			close(done)
		}()

		for _, test := range tests {
			for try := 1; ; try++ {
				select {
				case <-stop:
					test.logf("cancelled")
					return
				default:
				}

				if try > *retryMax {
					// Every try failed.
					test.logf("retry limit exceeded")
					failed++
					break
				}

				test.logf("running (try %v/%v)...", try, *retryMax)

				// Make a unique VTDATAROOT.
				dataDir, err := ioutil.TempDir(vtDataRoot, "vt_")
				if err != nil {
					test.logf("Failed to create temporary subdir in VTDATAROOT: %v", vtDataRoot)
					failed++
					break
				}

				// Run the test.
				start := time.Now()
				output, err := test.run(vtTop, dataDir)
				duration := time.Since(start)

				// Save/print test output.
				if err != nil || *logPass {
					if *printLog {
						test.logf("%s\n", output)
					}
					outFile := fmt.Sprintf("%v-%v.%v.log", test.name, test.runIndex+1, try)
					test.logf("saving test output to %v", outFile)
					if fileErr := ioutil.WriteFile(path.Join(outDir, outFile), output, os.FileMode(0644)); fileErr != nil {
						test.logf("WriteFile error: %v", fileErr)
					}
				}

				// Clean up the unique VTDATAROOT.
				if !*keepData {
					if err := os.RemoveAll(dataDir); err != nil {
						test.logf("WARNING: can't remove temporary VTDATAROOT: ", err)
					}
				}

				if err != nil {
					// This try failed.
					test.logf("FAILED (try %v/%v) in %v: %v", try, *retryMax, duration, err)
					testFailed(test.name)
					continue
				}

				testPassed(test.name, duration)

				if try == 1 {
					// Passed on the first try.
					test.logf("PASSED in %v", duration)
					passed++
				} else {
					// Passed, but not on the first try.
					test.logf("FLAKY (1/%v passed in %v)", try, duration)
					flaky++
					testFlaked(test.name, try)
				}
				break
			}
		}
	}()

	// Stop the loop and kill child processes if we get a signal.
	select {
	case <-sigchan:
		log.Printf("received signal, quitting")
		// Stop the test loop and wait for it to quit.
		close(stop)
		<-done
		// Terminate all existing tests.
		for _, t := range tests {
			t.stop()
		}
	case <-done:
	}

	// Clean up temp dir.
	if tmpDir != "" {
		log.Printf("Removing temp dir %v", tmpDir)
		if err := os.RemoveAll(tmpDir); err != nil {
			log.Printf("Failed to remove temp dir: %v", err)
		}
	}

	// Print summary.
	log.Printf(strings.Repeat("=", 50))
	for _, t := range tests {
		switch {
		case t.pass > 0 && t.fail == 0:
			log.Printf("%-32s\tPASS", t.name)
		case t.pass > 0 && t.fail > 0:
			log.Printf("%-32s\tFLAKY (%v/%v failed)", t.name, t.fail, t.pass+t.fail)
		case t.pass == 0 && t.fail > 0:
			log.Printf("%-32s\tFAIL (%v tries)", t.name, t.fail)
		case t.pass == 0 && t.fail == 0:
			log.Printf("%-32s\tSKIPPED", t.name)
		}
	}
	log.Printf(strings.Repeat("=", 50))
	skipped := len(tests) - passed - flaky - failed
	log.Printf("%v PASSED, %v FLAKY, %v FAILED, %v SKIPPED", passed, flaky, failed, skipped)
	log.Printf("Total time: %v", time.Since(startTime))

	if failed > 0 || skipped > 0 {
		os.Exit(1)
	}
}

func updateEnv(orig []string, updates map[string]string) []string {
	var env []string
	for _, v := range orig {
		parts := strings.SplitN(v, "=", 2)
		if _, ok := updates[parts[0]]; !ok {
			env = append(env, v)
		}
	}
	for k, v := range updates {
		env = append(env, k+"="+v)
	}
	return env
}

type Stats struct {
	TestStats map[string]TestStats
}

type TestStats struct {
	Pass, Fail, Flake int
	PassTime          time.Duration

	name string
}

func sendStats(values url.Values) {
	if *remoteStats != "" {
		log.Printf("Sending remote stats to %v", *remoteStats)
		if _, err := http.PostForm(*remoteStats, values); err != nil {
			log.Printf("Can't send remote stats: %v", err)
		}
	}
}

func testPassed(name string, passTime time.Duration) {
	sendStats(url.Values{
		"test":     {name},
		"result":   {"pass"},
		"duration": {passTime.String()},
	})
	updateTestStats(name, func(ts *TestStats) {
		totalTime := int64(ts.PassTime)*int64(ts.Pass) + int64(passTime)
		ts.Pass++
		ts.PassTime = time.Duration(totalTime / int64(ts.Pass))
	})
}

func testFailed(name string) {
	sendStats(url.Values{
		"test":   {name},
		"result": {"fail"},
	})
	updateTestStats(name, func(ts *TestStats) {
		ts.Fail++
	})
}

func testFlaked(name string, try int) {
	sendStats(url.Values{
		"test":   {name},
		"result": {"flake"},
		"try":    {strconv.FormatInt(int64(try), 10)},
	})
	updateTestStats(name, func(ts *TestStats) {
		ts.Flake += try - 1
	})
}

func updateTestStats(name string, update func(*TestStats)) {
	var stats Stats

	data, err := ioutil.ReadFile(statsFileName)
	if err != nil {
		log.Print("Can't read stats file, starting new one.")
	} else {
		if err := json.Unmarshal(data, &stats); err != nil {
			log.Printf("Can't parse stats file: %v", err)
			return
		}
	}

	if stats.TestStats == nil {
		stats.TestStats = make(map[string]TestStats)
	}
	ts := stats.TestStats[name]
	update(&ts)
	stats.TestStats[name] = ts

	data, err = json.MarshalIndent(stats, "", "\t")
	if err != nil {
		log.Printf("Can't encode stats file: %v", err)
		return
	}
	if err := ioutil.WriteFile(statsFileName, data, 0644); err != nil {
		log.Printf("Can't write stats file: %v", err)
	}
}

func reshardTests(config *Config, numShards int) error {
	var stats Stats

	var data []byte
	if *remoteStats != "" {
		log.Printf("Using remote stats for resharding: %v", *remoteStats)
		resp, err := http.Get(*remoteStats)
		if err != nil {
			return err
		}
		if data, err = ioutil.ReadAll(resp.Body); err != nil {
			return err
		}
	} else {
		var err error
		data, err = ioutil.ReadFile(statsFileName)
		if err != nil {
			return errors.New("can't read stats file")
		}
	}

	if err := json.Unmarshal(data, &stats); err != nil {
		return fmt.Errorf("can't parse stats file: %v", err)
	}

	// Sort tests by PassTime.
	var tests []TestStats
	var totalTime int64
	for name, test := range stats.TestStats {
		test.name = name
		tests = append(tests, test)
		totalTime += int64(test.PassTime)
	}
	sort.Sort(ByPassTime(tests))

	// Group into shards.
	max := totalTime / int64(numShards)
	shards := make([][]TestStats, numShards)
	sums := make([]int64, numShards)
	// First pass: greedy approximation.
	for len(tests) > 0 {
		v := int64(tests[0].PassTime)

		found := false
		for n := range shards {
			if sums[n]+v < max {
				shards[n] = append(shards[n], tests[0])
				sums[n] += v
				tests = tests[1:]
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	// Second pass: distribute the remainder.
	for len(tests) > 0 {
		nmin := 0
		min := sums[0]

		for n := range sums {
			if sums[n] < min {
				nmin = n
				min = sums[n]
			}
		}

		shards[nmin] = append(shards[nmin], tests[0])
		sums[nmin] += int64(tests[0].PassTime)
		tests = tests[1:]
	}

	// Update config and print results.
	for i, tests := range shards {
		for _, t := range tests {
			config.Tests[t.name].Shard = i
			log.Printf("% 32v:\t%v\n", t.name, t.PassTime)
		}
		log.Printf("Shard %v total: %v\n", i, time.Duration(sums[i]))
	}

	return nil
}

type ByPassTime []TestStats

func (a ByPassTime) Len() int           { return len(a) }
func (a ByPassTime) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByPassTime) Less(i, j int) bool { return a[i].PassTime > a[j].PassTime }

func getTestsSorted(names []string, testMap map[string]*Test) []*Test {
	sort.Strings(names)
	var tests []*Test
	for _, name := range names {
		t := testMap[name]
		t.name = name
		tests = append(tests, t)
	}
	return tests
}

func selectedTests(config *Config) []*Test {
	var tests []*Test
	if *shard >= 0 {
		// Run the tests in a given shard.
		// This can be combined with positional args.
		var names []string
		for name, t := range config.Tests {
			if t.Shard == *shard {
				t.name = name
				names = append(names, name)
			}
		}
		tests = getTestsSorted(names, config.Tests)
	}
	if flag.NArg() > 0 {
		// Positional args for manual selection.
		for _, name := range flag.Args() {
			t, ok := config.Tests[name]
			if !ok {
				log.Fatalf("Unknown test: %v", name)
			}
			t.name = name
			tests = append(tests, t)
		}
	}
	if flag.NArg() == 0 && *shard < 0 {
		// Run all tests.
		var names []string
		for name := range config.Tests {
			if !config.Tests[name].Manual {
				names = append(names, name)
			}
		}
		tests = getTestsSorted(names, config.Tests)
	}
	return tests
}
