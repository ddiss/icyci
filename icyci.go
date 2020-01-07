// SPDX-License-Identifier: AGPL-3.0-only
//
// Copyright (C) 2019 SUSE LLC

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"
)

func usage() {
	fmt.Printf("Usage: %s [options]\n", filepath.Base(os.Args[0]))
	flag.PrintDefaults()
}

type cliParams struct {
	sourceUrl      *url.URL
	sourceBranch   string
	testScript     string
	resultsUrl     *url.URL
	pushSrcToRslts bool
	pollIntervalS  uint64
}

type State int

const (
	uninit State = iota
	clone
	verify
	lock
	run
	push
	cleanup
	poll
)

type loopState struct {
	state           State
	transitionTimer *time.Timer
	verifiedTag     string
}

type stateDesc struct {
	desc    string
	timeout time.Duration
}

var states = map[State]stateDesc{
	uninit:  {"uninitialized", 0},
	clone:   {"clone source repository", time.Duration(10 * time.Minute)},
	verify:  {"verify branch HEAD", time.Duration(1 * time.Minute)},
	lock:    {"lock commit for testing", time.Duration(5 * time.Minute)},
	run:     {"run test", time.Duration(2 * time.Hour)},
	push:    {"push test output notes", time.Duration(10 * time.Minute)},
	cleanup: {"cleanup test artifacts", time.Duration(5 * time.Minute)},
	poll:    {"poll source for new commits", 0},
}

const (
	lockNotesRef   = "refs/notes/icyci.locked"
	stdoutNotesRef = "refs/notes/icyci.output.stdout"
	stderrNotesRef = "refs/notes/icyci.output.stderr"
	passedNotesRef = "refs/notes/icyci.result.passed"
	failedNotesRef = "refs/notes/icyci.result.failed"
	resultsRemote  = "results"
)

func cloneRepo(ch chan<- error, workDir string, sUrl *url.URL,
	branch string, rUrl *url.URL, targetDir string) {
	// TODO branch is not sanitized. Ignore submodules for now.
	if branch == "" {
		log.Fatal("empty branch name")
	}
	gitArgs := []string{"clone", "--no-checkout", "--single-branch",
		"--branch", branch, sUrl.String(), targetDir}
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = workDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Printf("git %v failed: %v", gitArgs, err)
		goto err_out
	}

	cmd = exec.Command("git", "remote", "add", resultsRemote,
		rUrl.String())
	cmd.Dir = targetDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		log.Printf("git failed: %v", err)
		goto err_out
	}
err_out:
	ch <- err
}

func verifyTag(sourceDir string, branch string, tag string) error {
	log.Printf("GPG verifying tag %s at origin/%s", tag, branch)

	cmd := exec.Command("git", "tag", "--verify", tag)
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func verifyCommit(sourceDir string, branch string) error {
	log.Printf("GPG verifying commit at origin/%s tip\n", branch)

	cmd := exec.Command("git", "verify-commit", "origin/"+branch)
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

type verifyCompletion struct {
	err error
	tag string
}

// check for a signed tag or commit at branch tip
func verifyRepo(ch chan<- verifyCompletion, sourceDir string, branch string) {
	tag := ""
	var describeOut bytes.Buffer
	cmd := exec.Command("git", "describe", "--tags", "--exact-match",
		"origin/"+branch)
	cmd.Dir = sourceDir
	cmd.Stdout = &describeOut
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		// if there are >=2 tags at HEAD, git-describe outputs only one.
		// If both unsigned and signed tags exist, git-describe outputs
		// a signed tag.
		tag = string(bytes.TrimRight(describeOut.Bytes(), "\n"))
		err = verifyTag(sourceDir, branch, tag)
		if err != nil {
			log.Printf("GPG verification of tag %s at origin/%s failed",
				tag, branch)
			goto err_out
		}
	} else {
		err = verifyCommit(sourceDir, branch)
		if err != nil {
			log.Printf("GPG verification of commit at origin/%s failed",
				branch)
			goto err_out
		}
	}
err_out:
	ch <- verifyCompletion{err, tag}
}

// lock to flags us as owner for testing this commit
func pushLock(ch chan<- error, sourceDir string, branch string) {
	var err error = nil

	for retries := 10; retries > 0; retries-- {

		// fetch may fail if no lock has ever been pushed
		cmd := exec.Command("git", "fetch", resultsRemote,
			"+"+lockNotesRef+":"+lockNotesRef)
		cmd.Dir = sourceDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
		if err != nil {
			log.Print("ignoring failure to fetch git notes lock")
		}

		// Without -f, "add" will fail if an existing note exists for
		// this commit. Failure indicates that the commit has been
		// tested or is currently being tested by another instance.
		cmd = exec.Command(
			"git", "notes", "--ref", lockNotesRef, "add", "-m",
			time.Now().String()+": this commit is being tested by icyCI",
			"origin/"+branch)
		cmd.Dir = sourceDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
		if err != nil {
			log.Printf("couldn't add git notes lock: %v", err)
			goto err_out
		}

		cmd = exec.Command("git", "push", resultsRemote, lockNotesRef)
		cmd.Dir = sourceDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
		if err == nil {
			log.Printf("successfully pushed git notes lock")
			break
		}

		if retries > 1 {
			log.Printf("retrying git notes lock after sleep")
			time.Sleep(1 * time.Second)
		}
	}
err_out:
	log.Printf("leaving push lock with err: %v", err)
	ch <- err
}

type runScriptCompletion struct {
	stdoutF      string
	stderrF      string
	summaryF     string
	scriptStatus error
	err          error
}

func runScript(ch chan<- runScriptCompletion, workDir string, sourceDir string,
	branch string, testScript string) {
	var stdoutF *os.File
	var stderrF *os.File
	var msg string
	cmpl := runScriptCompletion{}

	cmd := exec.Command("git", "checkout", "--quiet", "origin/"+branch)
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Printf("git failed: %v", err)
		goto err_out
	}

	cmpl.stdoutF = path.Join(workDir, "script.stdout")
	stdoutF, err = os.Create(cmpl.stdoutF)
	if err != nil {
		log.Printf("stdout log creation failed: %v", err)
		goto err_out
	}
	defer stdoutF.Close()

	cmpl.stderrF = path.Join(workDir, "script.stderr")
	stderrF, err = os.Create(cmpl.stderrF)
	if err != nil {
		log.Printf("stderr log creation failed: %v", err)
		goto err_out
	}
	defer stderrF.Close()

	cmd = exec.Command(testScript)
	cmd.Dir = sourceDir
	cmd.Stdout = stdoutF
	cmd.Stderr = stderrF
	err = cmd.Start()
	if err != nil {
		log.Printf("test %s failed to start: %v", testScript, err)
		goto err_out
	}

	cmpl.scriptStatus = cmd.Wait()
	stdoutF.Sync()
	stderrF.Sync()

	cmpl.summaryF = path.Join(workDir, "script.summary")
	if cmpl.scriptStatus == nil {
		msg = fmt.Sprintf("%s completed successfully", testScript)
	} else {
		msg = fmt.Sprintf("%s failed: %v\nSee %s and %s for details",
			testScript, cmpl.scriptStatus,
			stdoutNotesRef, stderrNotesRef)
	}
	log.Print(msg + "\n")
	err = ioutil.WriteFile(cmpl.summaryF, []byte(msg), os.FileMode(0644))
	if err != nil {
		goto err_out
	}

	ch <- cmpl
	return
err_out:
	ch <- runScriptCompletion{
		stdoutF:      "",
		stderrF:      "",
		summaryF:     "",
		scriptStatus: err,
		err:          err}
}

// push captured stdout and stderr notes to the results repository.
func pushResults(ch chan<- error, sourceDir string,
	branch string, tag string, pushSrcToRslts bool,
	cmpl runScriptCompletion) {

	var err error = nil
	type notesOut struct {
		ns  string
		msg string
	}
	var res notesOut

	if cmpl.scriptStatus == nil {
		res = notesOut{ns: passedNotesRef, msg: cmpl.summaryF}
	} else {
		res = notesOut{ns: failedNotesRef, msg: cmpl.summaryF}
	}

	for retries := 10; retries > 0; retries-- {
		// fetch ensures we don't conflict, may fail if never pushed
		cmd := exec.Command("git", "fetch", resultsRemote,
			"+"+stdoutNotesRef+":"+stdoutNotesRef,
			"+"+stderrNotesRef+":"+stderrNotesRef,
			"+"+res.ns+":"+res.ns)
		cmd.Dir = sourceDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Run()
		if err != nil {
			log.Print("ignoring failure to fetch output git notes")
		}

		for _, note := range []notesOut{res,
			{ns: stdoutNotesRef, msg: cmpl.stdoutF},
			{ns: stderrNotesRef, msg: cmpl.stderrF}} {

			gitArgs := []string{"notes", "--ref", note.ns, "add",
				"--allow-empty", "-F", note.msg,
				"origin/" + branch}
			cmd := exec.Command("git", gitArgs...)
			cmd.Dir = sourceDir
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			err = cmd.Run()
			if err != nil {
				log.Printf("git %v failed: %v", gitArgs, err)
				goto err_out
			}
		}

		gitArgs := []string{"push", resultsRemote, res.ns,
			stdoutNotesRef, stderrNotesRef}
		if pushSrcToRslts {
			gitArgs = append(gitArgs, "HEAD:refs/heads/"+branch)
			if tag != "" {
				gitArgs = append(gitArgs, "refs/tags/"+tag)
			}
		}
		cmd = exec.Command("git", gitArgs...)
		cmd.Dir = sourceDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
		if err == nil {
			log.Printf("successfully pushed git notes lock")
			break
		}

		if retries > 1 {
			log.Printf("retrying output git notes after sleep")
			time.Sleep(1 * time.Second)
		}
	}
err_out:
	ch <- err
}

func cleanupSource(ch chan<- error, sourceDir string) {
	cmd := exec.Command("git", "clean", "--quiet", "--force", "-xd")
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Printf("git failed: %v", err)
		goto err_out
	}
err_out:
	ch <- err
}

func pollGetRev(sourceDir string, branch string) (string, error) {
	var revParseOut bytes.Buffer
	curRev := ""

	cmd := exec.Command("git", "rev-parse", "origin/"+branch)
	cmd.Dir = sourceDir
	cmd.Stdout = &revParseOut
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Printf("git rev-parse failed: %v\n", err)
		goto err_out
	}

	curRev = string(bytes.TrimRight(revParseOut.Bytes(), "\n"))
err_out:
	return curRev, err
}

func pollFetch(sourceDir string, branch string) error {
	cmd := exec.Command("git", "fetch", "origin")
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Printf("git fetch failed: %v\n", err)
		goto err_out
	}
err_out:
	return err
}

func pollSource(retCh chan<- error, exitCh chan bool, sourceDir string, branch string,
	pollIntervalS uint64) {

	var err error = nil
	var pollTimer *time.Timer
	consecutiveFetchFail := 0
	const maxConsecutiveFetchFails = 10

	preFetchRev, err := pollGetRev(sourceDir, branch)
	if err != nil {
		log.Printf("failed to get pre-fetch revision: %v\n", err)
		goto err_out
	}
	log.Printf("Entering poll loop awaiting new %s commits at %s\n",
		branch, preFetchRev)

	pollTimer = time.NewTimer(time.Second * time.Duration(pollIntervalS))
	for {
		select {
		case <-exitCh:
			// event loop sends msg
			err = errors.New("exit requested")
			goto err_out

		case <-pollTimer.C:
			pollTimer.Reset(time.Second * time.Duration(pollIntervalS))

			err = pollFetch(sourceDir, branch)
			if err != nil {
				consecutiveFetchFail++
				if consecutiveFetchFail > maxConsecutiveFetchFails {
					goto err_out
				}
				log.Printf("Retrying; %d of maximum %d fetch failures\n",
					consecutiveFetchFail, maxConsecutiveFetchFails)
				continue
			}
			consecutiveFetchFail = 0

			postFetchRev, err := pollGetRev(sourceDir, branch)
			if err != nil {
				goto err_out
			}
			if preFetchRev != postFetchRev {
				log.Printf("detected branch %s change %s -> %s\n",
					branch, preFetchRev, postFetchRev)
				err = nil
				goto err_out
			}
		}
	}
err_out:
	retCh <- err
}

func transitionState(newState State, ls *loopState) {
	oldState := ls.state
	ls.state = newState

	_, exists := states[newState]
	if !exists {
		log.Fatalf("no states entry for %d\n", newState)
	}

	log.Printf("transitioning from state %d: %s -> %d: %s\n",
		oldState, states[oldState].desc,
		newState, states[newState].desc)

	if oldState == uninit {
		ls.transitionTimer = time.NewTimer(states[newState].timeout)
		return
	}

	// don't stop timer if leaving poll state, because...
	if oldState != poll && !ls.transitionTimer.Stop() {
		<-ls.transitionTimer.C
	}

	// ...there's no timeout for poll state
	if newState != poll {
		ls.transitionTimer.Reset(states[newState].timeout)
	}
}

// event loop to track state of clone, verify, test, push progress
func eventLoop(params *cliParams, workDir string, exitChan chan int) {

	sourceDir := path.Join(workDir, "source")

	// TODO reuse channels instead of creating one per activity
	cloneChan := make(chan error)
	verifyChan := make(chan verifyCompletion)
	pushLockChan := make(chan error)
	runScriptChan := make(chan runScriptCompletion)
	pushResultsChan := make(chan error)
	cleanupChan := make(chan error)
	pollRetChan := make(chan error)
	pollExitChan := make(chan bool)

	var ls loopState
	transitionState(clone, &ls)
	go func() {
		cloneRepo(cloneChan, workDir, params.sourceUrl,
			params.sourceBranch, params.resultsUrl, sourceDir)
	}()

	for {
		select {
		case cloneErr := <-cloneChan:
			if cloneErr != nil {
				log.Fatal(cloneErr)
			}
			log.Print("clone completed successfully\n")

			transitionState(verify, &ls)
			go func() {
				verifyRepo(verifyChan, sourceDir,
					params.sourceBranch)
			}()
		case verifyCmpl := <-verifyChan:
			ls.verifiedTag = ""
			if verifyCmpl.err != nil {
				transitionState(poll, &ls)
				go func() {
					pollSource(pollRetChan, pollExitChan,
						sourceDir,
						params.sourceBranch,
						params.pollIntervalS)
				}()
				continue
			}
			ls.verifiedTag = verifyCmpl.tag
			log.Print("verify completed successfully\n")

			transitionState(lock, &ls)
			go func() {
				pushLock(pushLockChan, sourceDir,
					params.sourceBranch)
			}()

		case pushLockErr := <-pushLockChan:
			if pushLockErr != nil {
				transitionState(poll, &ls)
				go func() {
					pollSource(pollRetChan, pollExitChan,
						sourceDir,
						params.sourceBranch,
						params.pollIntervalS)
				}()
				continue
			}
			transitionState(run, &ls)
			go func() {
				runScript(runScriptChan, workDir, sourceDir,
					params.sourceBranch, params.testScript)
			}()
		case runScriptCmpl := <-runScriptChan:
			if runScriptCmpl.err != nil {
				log.Fatal(runScriptCmpl.err)
			}

			transitionState(push, &ls)
			go func() {
				pushResults(pushResultsChan, sourceDir,
					params.sourceBranch, ls.verifiedTag,
					params.pushSrcToRslts, runScriptCmpl)
			}()
		case pushResultsErr := <-pushResultsChan:
			if pushResultsErr != nil {
				log.Fatal(pushResultsErr)
			}
			log.Print("git push completed successfully\n")

			transitionState(cleanup, &ls)
			go func() {
				cleanupSource(cleanupChan, sourceDir)
			}()
		case cleanupErr := <-cleanupChan:
			if cleanupErr != nil {
				log.Fatal(cleanupErr)
			}
			log.Print("cleanup completed successfully\n")

			transitionState(poll, &ls)
			go func() {
				pollSource(pollRetChan, pollExitChan,
					sourceDir,
					params.sourceBranch,
					params.pollIntervalS)
			}()
		case pollErr := <-pollRetChan:
			if pollErr != nil {
				log.Fatal(pollErr)
			}
			log.Print("poll / fetch loop returned success\n")

			transitionState(verify, &ls)
			go func() {
				verifyRepo(verifyChan, sourceDir,
					params.sourceBranch)
			}()
		case <-ls.transitionTimer.C:
			log.Fatalf("State %v transition timeout!", ls.state)
		case <-exitChan:
			log.Printf("Got exit message while in state %d\n",
				ls.state)
			if ls.state == poll {
				pollExitChan <- true
				log.Print("waiting for poll routine to exit\n")
				<-pollRetChan
			}
			return
		}
	}
}

func main() {
	var srcRawUrl string
	var resultsRawUrl string
	var err error

	params := new(cliParams)
	flag.Usage = usage
	flag.StringVar(&srcRawUrl, "source-repo", "",
		"Git `URL` for the repository under test (required)")
	flag.StringVar(&params.sourceBranch, "source-branch", "master",
		"Git `branch` for the repository under test")
	flag.StringVar(&params.testScript, "test-script", "",
		"Test `script path`, relative to source-repo or absolute (required)")
	flag.StringVar(&resultsRawUrl, "results-repo", "",
		"Git `URL` to push test results to (required)")
	// If the corresponding source branch is not pushed, then cloning the
	// results repository only (if different to source) may lead to
	// confusion due to the missing commits referenced by the notes.
	flag.BoolVar(&params.pushSrcToRslts, "push-source-to-results", true,
		"Push source-branch and any tag to results-repo, in addition to notes")
	flag.Uint64Var(&params.pollIntervalS, "poll-interval", 60,
		"While idle, poll source-repo for changes at this `seconds` interval")
	flag.Parse()

	if srcRawUrl == "" || resultsRawUrl == "" || params.testScript == "" {
		usage()
		return
	}

	params.sourceUrl, err = url.Parse(srcRawUrl)
	if err != nil {
		log.Fatalf("failed to parse URL \"%s\": %v\n",
			srcRawUrl, err)
	}

	params.resultsUrl, err = url.Parse(resultsRawUrl)
	if err != nil {
		log.Fatalf("failed to parse URL \"%s\": %v\n",
			resultsRawUrl, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	// create a directory to use for staging source, etc.
	wdir, err := ioutil.TempDir("", "icyci-workspace")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(wdir)

	err = os.Chdir(wdir)
	if err != nil {
		log.Fatal(err)
	}
	defer os.Chdir(cwd)

	eventLoop(params, wdir, nil)
}
