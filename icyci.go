// SPDX-License-Identifier: AGPL-3.0-only
//
// Copyright (C) 2019-2022 SUSE LLC

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func usage() {
	fmt.Printf("Usage: %s [options]\n", filepath.Base(os.Args[0]))
	flag.PrintDefaults()
}

var buildVer string // set via -ldflags "-X main.buildVer=<version>"

func vers() {
	ver := buildVer
	if ver == "" {
		ver = "version unknown"
	}

	fmt.Printf("icyCI %s\n", ver)
}

type cliParams struct {
	sourceUrl       *url.URL
	sourceBranch    string
	testScript      string
	resultsUrl      *url.URL
	pushSrcToRslts  bool
	pollIntervalS   uint64
	disableTimeouts bool
	notesNS         string
}

type State int

const (
	uninit State = iota
	clone
	verify
	lock
	startCmd
	awaitCmd
	push
	cleanup
	poll
)

type loopState struct {
	state           State
	transitionTimer *time.Timer
	verifiedTag     string
	disableTimeouts bool
}

type stateDesc struct {
	desc    string
	timeout time.Duration
}

var states = map[State]stateDesc{
	uninit:   {"uninitialized", 0},
	clone:    {"clone source repository", time.Duration(1 * time.Hour)},
	verify:   {"verify branch HEAD", time.Duration(1 * time.Minute)},
	lock:     {"lock commit for testing", time.Duration(10 * time.Minute)},
	startCmd: {"start test command", time.Duration(1 * time.Minute)},
	awaitCmd: {"await test completion", time.Duration(2 * time.Hour)},
	push:     {"push test output notes", time.Duration(1 * time.Hour)},
	cleanup:  {"cleanup test artifacts", time.Duration(10 * time.Minute)},
	poll:     {"poll source for new commits", 0},
}

const (
	// notes refs are preceeded by a namespace, which defaults to:
	defNotesNS  = "icyci"
	lockNotes   = ".locked"
	stdoutNotes = ".output.stdout"
	stderrNotes = ".output.stderr"
	passedNotes = ".result.passed"
	failedNotes = ".result.failed"
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
func pushLock(ch chan<- error, sourceDir string, branch string, notesNS string) {
	var err error = nil
	lockNotesRef := "refs/notes/"+notesNS+lockNotes

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
	ch <- err
}

type runCmdState struct {
	cmd          *exec.Cmd
	stdoutP      string
	stdoutF      *os.File
	stderrP      string
	stderrF      *os.File
	summaryP     string
	scriptStatus error
	err          error
}

func startCommand(ch chan<- runCmdState, workDir string, sourceDir string,
	branch string, testScript string) {
	cmpl := runCmdState{}

	cmd := exec.Command("git", "checkout", "--quiet", "origin/"+branch)
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Printf("git failed: %v", err)
		goto err_out
	}

	cmpl.stdoutP = path.Join(workDir, "script.stdout")
	cmpl.stdoutF, err = os.Create(cmpl.stdoutP)
	if err != nil {
		log.Printf("stdout log creation failed: %v", err)
		goto err_out
	}

	cmpl.stderrP = path.Join(workDir, "script.stderr")
	cmpl.stderrF, err = os.Create(cmpl.stderrP)
	if err != nil {
		log.Printf("stderr log creation failed: %v", err)
		goto err_stdout_close
	}

	cmpl.summaryP = path.Join(workDir, "script.summary")

	cmd = exec.Command(testScript)

	cmd.Env = append(os.Environ(), "ICYCI_PID="+strconv.Itoa(os.Getpid()))
	cmd.Dir = sourceDir
	cmd.Stdout = io.MultiWriter(os.Stdout, cmpl.stdoutF)
	cmd.Stderr = io.MultiWriter(os.Stderr, cmpl.stderrF)
	err = cmd.Start()
	if err != nil {
		log.Printf("test %s failed to start: %v", testScript, err)
		goto err_stderr_close
	}
	cmpl.cmd = cmd

	ch <- cmpl
	return

err_stderr_close:
	cmpl.stderrF.Close()
err_stdout_close:
	cmpl.stdoutF.Close()
err_out:
	ch <- runCmdState{
		cmd:          nil,
		stdoutP:      "",
		stdoutF:      nil,
		stderrP:      "",
		stderrF:      nil,
		summaryP:     "",
		scriptStatus: err,
		err:          err}
}

func awaitCommand(ch chan<- runCmdState, notesNS string, cmdState *runCmdState) {
	var msg string

	cmdState.scriptStatus = cmdState.cmd.Wait()
	cmdState.stdoutF.Sync()
	cmdState.stdoutF.Close()
	cmdState.stderrF.Sync()
	cmdState.stderrF.Close()

	if cmdState.scriptStatus == nil {
		msg = fmt.Sprintf("%s completed successfully",
			cmdState.cmd.Path)
	} else {
		stdoutNotesRef := "refs/notes/"+notesNS+stdoutNotes
		stderrNotesRef := "refs/notes/"+notesNS+stderrNotes
		msg = fmt.Sprintf("%s failed: %v\nSee %s and %s for details",
			cmdState.cmd.Path, cmdState.scriptStatus,
			stdoutNotesRef, stderrNotesRef)
	}
	log.Print(msg + "\n")
	err := ioutil.WriteFile(cmdState.summaryP, []byte(msg), os.FileMode(0644))
	if err != nil {
		goto err_out
	}

	ch <- *cmdState
	return
err_out:
	ch <- runCmdState{
		cmd:          nil,
		stdoutP:      "",
		stdoutF:      nil,
		stderrP:      "",
		stderrF:      nil,
		summaryP:     "",
		scriptStatus: err,
		err:          err}
}

// push captured stdout and stderr notes to the results repository.
func pushResults(ch chan<- error, sourceDir string,
	branch string, tag string, pushSrcToRslts bool, notesNS string,
	cmpl runCmdState) {

	var err error = nil
	type notesOut struct {
		ns  string
		msg string
	}
	var res notesOut
	stdoutNotesRef := "refs/notes/"+notesNS+stdoutNotes
	stderrNotesRef := "refs/notes/"+notesNS+stderrNotes
	var notesName string

	if cmpl.scriptStatus == nil {
		notesName = passedNotes
	} else {
		notesName = failedNotes
	}
	res = notesOut{ns: "refs/notes/"+notesNS+notesName, msg: cmpl.summaryP}

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
			{ns: stdoutNotesRef, msg: cmpl.stdoutP},
			{ns: stderrNotesRef, msg: cmpl.stderrP}} {

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
			// force push because 'branch' we fetched previously
			// may be rebased, i.e. non-fast-forward
			gitArgs = append(gitArgs, "+HEAD:refs/heads/"+branch)
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
		log.Printf("git %v failed: %v", gitArgs, err)

		if retries > 1 {
			log.Printf("retrying push after sleep")
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

// initial clone provided --single-branch, so fetch will ignore other heads.
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

	if !ls.disableTimeouts {
		if oldState == uninit {
			ls.transitionTimer = time.NewTimer(states[newState].timeout)
			return
		}

		// don't stop timer if leaving poll state, because...
		if oldState != poll && !ls.transitionTimer.Stop() {
			// select + default to avoid deadlock when already drained
			select {
				case <-ls.transitionTimer.C:
				default:
			}
		}

		// ...there's no timeout for poll state
		if newState != poll {
			ls.transitionTimer.Reset(states[newState].timeout)
		}
	}
}

// event loop to track state of clone, verify, test, push progress
func eventLoop(params *cliParams, workDir string, evExitChan chan int) {

	sourceDir := path.Join(workDir, "source")

	cmplChan := make(chan error)
	verifyChan := make(chan verifyCompletion)
	runCmdChan := make(chan runCmdState)
	childExitChan := make(chan bool)
	signalChan := make(chan os.Signal, 1)

	var ls loopState = loopState{disableTimeouts: params.disableTimeouts}
	transitionState(clone, &ls)
	go func() {
		cloneRepo(cmplChan, workDir, params.sourceUrl,
			params.sourceBranch, params.resultsUrl, sourceDir)
	}()

	for {
		select {
		// generic competion handler
		case err := <-cmplChan:
			if err != nil {
				log.Printf("%s failed with %s\n",
					states[ls.state].desc, err)
				if ls.state == lock {
					// lock errors transition to poll state
					transitionState(poll, &ls)
					go func() {
						pollSource(cmplChan,
							childExitChan,
							sourceDir,
							params.sourceBranch,
							params.pollIntervalS)
					}()
					continue
				}
				// other errors are fatal
				log.Fatal(err)
			}

			log.Printf("%s completed successfully\n",
				states[ls.state].desc)
			switch ls.state {
			case clone:
				transitionState(verify, &ls)
				go func() {
					verifyRepo(verifyChan, sourceDir,
						params.sourceBranch)
				}()
			// verify handled via separate chan, transitions to lock
			// state
			case lock:
				transitionState(startCmd, &ls)
				// accept signals to push notes prior to cmd
				// completion
				signal.Notify(signalChan, syscall.SIGUSR1)
				go func() {
					startCommand(runCmdChan, workDir,
						sourceDir, params.sourceBranch,
						params.testScript)
				}()
			// start/wait script completion handled via separate
			// chan, transitions to push state
			case push:
				transitionState(cleanup, &ls)
				go func() {
					cleanupSource(cmplChan, sourceDir)
				}()
			case cleanup:
				transitionState(poll, &ls)
				go func() {
					pollSource(cmplChan, childExitChan,
						sourceDir,
						params.sourceBranch,
						params.pollIntervalS)
				}()
			case poll:
				transitionState(verify, &ls)
				go func() {
					verifyRepo(verifyChan, sourceDir,
						params.sourceBranch)
				}()
			default:
				log.Fatalf("unhandled completion in state %s",
					states[ls.state].desc)
			}
		case verifyCmpl := <-verifyChan:
			ls.verifiedTag = ""
			if verifyCmpl.err != nil {
				transitionState(poll, &ls)
				go func() {
					pollSource(cmplChan, childExitChan,
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
				pushLock(cmplChan, sourceDir,
					params.sourceBranch, params.notesNS)
			}()

		case runCmdState := <-runCmdChan:
			if runCmdState.err != nil {
				log.Fatal(runCmdState.err)
			}

			switch ls.state {
			case startCmd:
				transitionState(awaitCmd, &ls)
				go func() {
					awaitCommand(runCmdChan, params.notesNS,
						&runCmdState)
				}()
			case awaitCmd:
				// stop accepting signals as on-demand push reqs
				signal.Stop(signalChan)
				transitionState(push, &ls)
				go func() {
					pushResults(cmplChan, sourceDir,
						params.sourceBranch, ls.verifiedTag,
						params.pushSrcToRslts,
						params.notesNS, runCmdState)
				}()
			}
		case <-ls.transitionTimer.C:
			log.Fatalf("State %v transition timeout!", ls.state)
		case s := <-signalChan:
			log.Printf("Got signal %d while in state %d\n",
				s, ls.state)
		case <-evExitChan:
			log.Printf("Got exit message while in state %d\n",
				ls.state)
			if ls.state == poll {
				childExitChan <- true
				log.Print("waiting for poll routine to exit\n")
				<-cmplChan
			}
			return
		}
	}
}

func parseStateTimeout(state_duration string) error {
	state_dur := strings.Split(state_duration, ":")
	if len(state_dur) != 2 {
		return errors.New("invalid state_duration parameter")
	}
	state := uninit
	dur, err := time.ParseDuration(state_dur[1])
	if err != nil {
		return err
	}

	switch state_dur[0] {
	case "await-command":
		state = awaitCmd
	default:
		log.Printf("-timeout state %s invalid. Supported: %s.\n",
			state_dur[0], "await-command");
		return errors.New("-timeout state invalid")
	}

	if s, ok := states[state]; ok {
		log.Printf("Changing %s timeout from %v to %v\n", state_dur[0],
			s.timeout, dur)
		s.timeout = dur
		states[state] = s
	}

	return nil
}

func main() {
	var srcRawUrl string
	var resultsRawUrl string
	var printVers bool
	var err error

	params := new(cliParams)
	flag.Usage = usage
	flag.StringVar(&srcRawUrl, "source-repo", "",
		"Git `URL` for the repository under test (required)")
	flag.StringVar(&params.sourceBranch, "source-branch", "main",
		"Git `branch` for the repository under test")
	flag.StringVar(&params.testScript, "test-script", "",
		"Test script `path`, relative to source-repo or absolute (required)")
	flag.StringVar(&resultsRawUrl, "results-repo", "",
		"Git `URL` to push test results to (required)")
	// If the corresponding source branch is not pushed, then cloning the
	// results repository only (if different to source) may lead to
	// confusion due to the missing commits referenced by the notes.
	flag.BoolVar(&params.pushSrcToRslts, "push-source-to-results", true,
		"Push source-branch and any tag to results-repo, in addition to notes")
	flag.Uint64Var(&params.pollIntervalS, "poll-interval", 60,
		"While idle, poll source-repo for changes at this `seconds` interval")
	flag.BoolVar(&printVers, "v", false, "print version and then exit")
	flag.BoolVar(&params.disableTimeouts, "disable-timeouts", false,
		"Disable timeouts for states transitions")
	flag.StringVar(&params.notesNS, "notes-ns", defNotesNS,
		"Namespace (`prefix`) to use for all git notes, e.g. \"icyci-riscv\"")
	flag.Func("timeout", "Timeout for a given state in `state:duration` format",
		parseStateTimeout)
	flag.Parse()

	if printVers {
		vers()
		return
	}

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
