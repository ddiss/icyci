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
	sourceRefUrl    *url.URL
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
	defNotesNS    = "icyci"
	lockNotes     = "locked"
	stdoutNotes   = "output.stdout"
	stderrNotes   = "output.stderr"
	passedNotes   = "result.passed"
	failedNotes   = "result.failed"
	resultsRemote = "results"
)

func cloneRepo(ch chan<- error, workDir string, sUrl *url.URL, sRefUrl *url.URL,
	branch string, rUrl *url.URL, targetDir string) {
	// TODO branch is not sanitized. Ignore submodules for now.
	if branch == "" {
		log.Panic("empty branch name")
	}
	gitArgs := []string{"clone", "--no-checkout", "--single-branch",
		"--branch", branch}
	if sRefUrl != nil {
		gitArgs = append(gitArgs, "--reference", sRefUrl.String())
	}
	gitArgs = append(gitArgs, sUrl.String(), targetDir)
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
	lockNotesRef := "refs/notes/" + notesNS + "." + lockNotes

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
	pgid         int
	cmdWaitChan  chan error
	stdoutP      string
	stdoutF      *os.File
	stderrP      string
	stderrF      *os.File
	scriptStatus error
	err          error
}

func startCommand(ch chan<- runCmdState, notesDir string, sourceDir string,
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

	if testScript == "" {
		ch <- cmpl
		return
	}

	cmpl.stdoutP = path.Join(notesDir, stdoutNotes)
	cmpl.stdoutF, err = os.Create(cmpl.stdoutP)
	if err != nil {
		log.Printf("stdout log creation failed: %v", err)
		goto err_out
	}

	cmpl.stderrP = path.Join(notesDir, stderrNotes)
	cmpl.stderrF, err = os.Create(cmpl.stderrP)
	if err != nil {
		log.Printf("stderr log creation failed: %v", err)
		goto err_stdout_close
	}

	cmpl.cmdWaitChan = make(chan error)
	go func() {
		cmd = exec.Command(testScript)
		cmd.Env = append(os.Environ(),
			"ICYCI_PID="+strconv.Itoa(os.Getpid()))
		cmd.Dir = sourceDir
		cmd.Stdout = io.MultiWriter(os.Stdout, cmpl.stdoutF)
		cmd.Stderr = io.MultiWriter(os.Stderr, cmpl.stderrF)
		// set Process Group ID to PID of the newly spawned process
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err := cmd.Start()
		if err != nil {
			log.Printf("test %s failed to start: %v", testScript, err)
			cmpl.scriptStatus = err
			ch <- cmpl
			cmpl.cmdWaitChan <- nil // awaitCommand retains err above
			return
		}
		cmpl.pgid = cmd.Process.Pid

		// notification of start goes through regular completion channel
		ch <- cmpl

		// notification of cmd completion goes via a separate channel
		// monitored by awaitCommand()...
		cmpl.cmdWaitChan <- cmd.Wait()
	}()
	return

err_stdout_close:
	cmpl.stdoutF.Close()
err_out:
	ch <- runCmdState{
		cmdWaitChan:  nil,
		stdoutP:      "",
		stdoutF:      nil,
		stderrP:      "",
		stderrF:      nil,
		scriptStatus: nil,
		err:          err}
}

// @exitCh can be used to kill the cmd process on timeout / exit
func awaitCommand(ch chan<- runCmdState, exitCh chan error, cmdPath string,
	notesDir string, notesNS string, cmdState *runCmdState) {
	var msg string
	var err error
	done := false

	if cmdState.cmdWaitChan == nil {
		done = true // no cmd to wait for
	}

	for !done {
		select {
		case cmdStatus := <-cmdState.cmdWaitChan:
			// don't overwrite exitCh error with Kill() status. Also
			// retain any cmd.Start() error.
			if cmdState.scriptStatus == nil {
				cmdState.scriptStatus = cmdStatus
			}
			done = true
		case err = <-exitCh:
			if cmdState.pgid == 0 {
				continue // not started
			}
			syscall.Kill(-cmdState.pgid, syscall.SIGKILL)
			log.Printf("killed PGID %d due to exit request: %v\n",
				cmdState.pgid, err)
			cmdState.scriptStatus = err
		}
	}
	cmdState.stdoutF.Sync()
	cmdState.stdoutF.Close()
	cmdState.stderrF.Sync()
	cmdState.stderrF.Close()

	var summaryP string
	if cmdState.scriptStatus == nil {
		if cmdPath == "" {
			msg = "No -test-script specified"
		} else {
			msg = fmt.Sprintf("%s completed successfully", cmdPath)
		}
		summaryP = path.Join(notesDir, passedNotes)
	} else {
		stdoutNotesRef := "refs/notes/" + notesNS + "." + stdoutNotes
		stderrNotesRef := "refs/notes/" + notesNS + "." + stderrNotes
		msg = fmt.Sprintf("%s failed: %v\nSee %s and %s for details",
			cmdPath, cmdState.scriptStatus,
			stdoutNotesRef, stderrNotesRef)
		summaryP = path.Join(notesDir, failedNotes)
	}
	log.Print(msg + "\n")
	err = ioutil.WriteFile(summaryP, []byte(msg), os.FileMode(0644))
	if err != nil {
		goto err_out
	}

	ch <- *cmdState
	return
err_out:
	ch <- runCmdState{
		stdoutP:      "",
		stdoutF:      nil,
		stderrP:      "",
		stderrF:      nil,
		scriptStatus: err,
		err:          err}
}

// push captured stdout and stderr notes to the results repository.
func pushResults(ch chan<- error, sourceDir string,
	branch string, tag string, pushSrcToRslts bool, notesDir string,
	notesNS string, cmpl runCmdState) {

	var err error = nil
	// file name is appended to notesNS for the pushed note name
	var allNotes []string

	if cmpl.scriptStatus == nil {
		allNotes = append(allNotes, passedNotes)
	} else {
		allNotes = append(allNotes, failedNotes)
	}

	if cmpl.stdoutP != "" {
		allNotes = append(allNotes, stdoutNotes)
	}
	if cmpl.stderrP != "" {
		allNotes = append(allNotes, stderrNotes)
	}

	gitFetchCmd := []string{"fetch", resultsRemote}
	gitPushCmd := []string{"push", resultsRemote}
	for _, note := range allNotes {
		notesRef := "refs/notes/" + notesNS + "." + note
		gitFetchCmd = append(gitFetchCmd, "+"+notesRef+":"+notesRef)
		gitPushCmd = append(gitPushCmd, notesRef)
	}
	if pushSrcToRslts {
		// force push because 'branch' we fetched previously
		// may be rebased, i.e. non-fast-forward
		gitPushCmd = append(gitPushCmd, "+HEAD:refs/heads/"+branch)
		if tag != "" {
			gitPushCmd = append(gitPushCmd, "refs/tags/"+tag)
		}
	}

	for retries := 10; retries > 0; retries-- {
		// fetch ensures we don't conflict, may fail if never pushed
		cmd := exec.Command("git", gitFetchCmd...)
		cmd.Dir = sourceDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Run()
		if err != nil {
			log.Print("ignoring failure to fetch output git notes")
		}

		for _, note := range allNotes {
			notesRef := "refs/notes/" + notesNS + "." + note
			gitArgs := []string{"notes", "--ref", notesRef, "add",
				"--allow-empty", "-F",
				path.Join(notesDir, note),
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

		cmd = exec.Command("git", gitPushCmd...)
		cmd.Dir = sourceDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
		if err == nil {
			log.Printf("successfully pushed git notes lock")
			break
		}
		log.Printf("git %v failed: %v", gitPushCmd, err)

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

func pollSource(retCh chan<- error, exitCh chan error, sourceDir string, branch string,
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
		case err = <-exitCh:
			// event loop exit requested
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
		log.Panicf("no states entry for %d\n", newState)
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
func eventLoop(params *cliParams, workDir string, evSigChan chan os.Signal) {

	sourceDir := path.Join(workDir, "source")
	// with the exception of the "lock", all notes content sent to the
	// results repo is staged in notesDir
	notesDir := path.Join(workDir, "notes")
	err := os.MkdirAll(notesDir, 0700)
	if err != nil {
		log.Panic(err)
	}

	cmplChan := make(chan error)
	verifyChan := make(chan verifyCompletion)
	runCmdChan := make(chan runCmdState)
	childExitChan := make(chan error)

	var ls loopState = loopState{disableTimeouts: params.disableTimeouts}
	transitionState(clone, &ls)
	go func() {
		cloneRepo(cmplChan, workDir, params.sourceUrl, params.sourceRefUrl,
			params.sourceBranch, params.resultsUrl, sourceDir)
	}()

	for {
		select {
		// generic competion handler
		case err = <-cmplChan:
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
				log.Panic(err)
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
				go func() {
					startCommand(runCmdChan, notesDir,
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
				log.Panicf("unhandled completion in state %s",
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
				log.Panic(runCmdState.err)
			}

			switch ls.state {
			case startCmd:
				transitionState(awaitCmd, &ls)
				go func() {
					awaitCommand(runCmdChan, childExitChan,
						params.testScript, notesDir,
						params.notesNS, &runCmdState)
				}()
			case awaitCmd:
				transitionState(push, &ls)
				go func() {
					pushResults(cmplChan, sourceDir,
						params.sourceBranch, ls.verifiedTag,
						params.pushSrcToRslts, notesDir,
						params.notesNS, runCmdState)
				}()
			}
		case <-ls.transitionTimer.C:
			if ls.state == awaitCmd {
				ls.transitionTimer.Stop()
				// job timeout isn't fatal. Invoke SIGKILL
				err := fmt.Errorf("%v await-command timeout reached",
					states[awaitCmd].timeout)
				childExitChan <- err
			} else {
				log.Panicf("State %v transition timeout!",
					ls.state)
			}
		case s := <-evSigChan:
			log.Printf("Got signal %d while in state %d\n",
				s, ls.state)
			if s != syscall.SIGTERM {
				log.Printf("unexpected signal: %v\n", s)
				continue
			}
			log.Printf("Got exit message while in state %d\n",
				ls.state)
			if ls.state == poll {
				childExitChan <- errors.New("exit requested")
				log.Print("waiting for poll routine to exit\n")
				<-cmplChan
			} else if ls.state == awaitCmd {
				childExitChan <- errors.New("exit requested")
				log.Print("waiting for cmd to exit via SIGKILL\n")
				<-runCmdChan
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
		log.Printf("-timeout state %s invalid. Supported: %s (default %v).\n",
			state_dur[0], "await-command", states[awaitCmd].timeout)
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

func parseCliArgs(exit func(int)) *cliParams {
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
		"Command `path` to run on verified branch, relative to "+
			"source-repo or absolute")
	flag.StringVar(&resultsRawUrl, "results-repo", "",
		"Git `URL` to push test results to (required)")
	// If the corresponding source branch is not pushed, then cloning the
	// results repository only (if different to source) may lead to
	// confusion due to the missing commits referenced by the notes.
	flag.BoolVar(&params.pushSrcToRslts, "push-source-to-results", true,
		"Push source-branch and any tag to results-repo, in addition to notes")
	// TODO should support time.ParseDuration() suffixes, e.g. 1h?
	flag.Uint64Var(&params.pollIntervalS, "poll-interval", 60,
		"While idle, poll source-repo for changes at this `seconds` interval")
	flag.BoolVar(&printVers, "v", false, "print version and then exit")
	flag.BoolVar(&params.disableTimeouts, "disable-timeouts", false,
		"Disable timeouts for states transitions")
	flag.StringVar(&params.notesNS, "notes-ns", defNotesNS,
		"Namespace (`prefix`) to use for all git notes, e.g. \"icyci-riscv\"")
	flag.Func("timeout", "Individual `state:duration` timeout. E.g. "+
		"\"await-command:1h\" fails a test script if runtime exceeds an hour",
		parseStateTimeout)
	flag.Func("source-reference",
		"Git clone reference `URL` to obtain objects from",
		func(s string) error {
			params.sourceRefUrl, err = url.Parse(s)
			if err != nil {
				log.Printf("invalid source-reference URL \"%s\": %v\n",
					s, err)
				exit(1)
			}
			return err
		})
	flag.Parse()
	if len(flag.Args()) > 0 {
		log.Printf("Unprocessed arguments: %v", flag.Args())
		exit(1)
	}

	if printVers {
		vers()
		exit(0)
	}

	if srcRawUrl == "" || resultsRawUrl == "" {
		usage()
		exit(1)
	}

	params.sourceUrl, err = url.Parse(srcRawUrl)
	if err != nil {
		log.Printf("failed to parse URL \"%s\": %v\n",
			srcRawUrl, err)
		exit(1)
	}

	params.resultsUrl, err = url.Parse(resultsRawUrl)
	if err != nil {
		log.Printf("failed to parse URL \"%s\": %v\n",
			resultsRawUrl, err)
		exit(1)
	}

	return params
}

func main() {
	params := parseCliArgs(func(exitCode int) {
		os.Exit(exitCode)
	})

	cwd, err := os.Getwd()
	if err != nil {
		log.Panic(err)
	}

	// create a directory to use for staging source, etc.
	wdir, err := ioutil.TempDir("", "icyci-workspace")
	if err != nil {
		log.Panic(err)
	}
	defer os.RemoveAll(wdir)

	err = os.Chdir(wdir)
	if err != nil {
		log.Panic(err)
	}
	defer os.Chdir(cwd)

	evSigChan := make(chan os.Signal)
	// TODO: SIGUSR1 requests notes push prior to cmd completion
	signal.Notify(evSigChan, syscall.SIGUSR1, syscall.SIGTERM)
	eventLoop(params, wdir, evSigChan)
	signal.Stop(evSigChan)
}
