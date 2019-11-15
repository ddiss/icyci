// SPDX-License-Identifier: AGPL-3.0-only
//
// Copyright (C) 2019 SUSE LLC

package main

import (
	"bytes"
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
	resultsBranch  string
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
	save
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
	save:    {"save test output as git notes", time.Duration(1 * time.Minute)},
	push:    {"push test output notes", time.Duration(10 * time.Minute)},
	cleanup: {"cleanup test artifacts", time.Duration(5 * time.Minute)},
	poll:    {"poll source for new commits", 0},
}

const lockNotesRef = "refs/notes/icyci.locked"
const stdoutNotesRef = "refs/notes/icyci.stdout"
const stderrNotesRef = "refs/notes/icyci.stderr"
const allNotesGlob = "refs/notes/*"

func cloneRepo(ch chan<- error, workDir string, u *url.URL,
	branch string, targetDir string) {
	// TODO branch is not sanitized. Ignore submodules for now.
	if branch == "" {
		log.Fatal("empty branch name")
	}
	gitArgs := []string{"clone", "--no-checkout", "--single-branch",
		"--branch", branch, u.String(), targetDir}
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = workDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Printf("git %v failed: %v", gitArgs, err)
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
func pushLock(ch chan<- error, sourceDir string, branch string,
	u *url.URL) {

	cmd := exec.Command("git", "notes", "--ref", lockNotesRef, "add", "-m",
		"this commit is being tested by icyCI",
		"origin/"+branch)
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Printf("failed to add git notes lock: %v", err)
		goto err_out
	}

	cmd = exec.Command("git", "push", u.String(), lockNotesRef)
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		log.Printf("failed to push git notes lock: %v", err)
		// delete local lock note if push failed
		cmd = exec.Command("git", "notes", "--ref", lockNotesRef,
			"remove", "origin/"+branch)
		cmd.Dir = sourceDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if cmd.Run() != nil {
			log.Print("ignoring failure to cancel git notes lock")
		}
		goto err_out
	}
err_out:
	ch <- err
}

type runScriptCompletion struct {
	stdoutFile   string
	stderrFile   string
	scriptStatus error
	err          error
}

func runScript(ch chan<- runScriptCompletion, workDir string, sourceDir string,
	branch string, testScript string) {
	var stdoutF *os.File
	var stderrF *os.File
	cmpl := runScriptCompletion{}

	cmd := exec.Command("git", "checkout", "--quiet", "origin/"+branch)
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Printf("git failed: %v", err)
		goto err_out
	}

	cmpl.stdoutFile = path.Join(workDir, "script.stdout")
	stdoutF, err = os.Create(cmpl.stdoutFile)
	if err != nil {
		log.Printf("stdout log creation failed: %v", err)
		goto err_out
	}
	defer stdoutF.Close()

	cmpl.stderrFile = path.Join(workDir, "script.stderr")
	stderrF, err = os.Create(cmpl.stderrFile)
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

	ch <- cmpl
	return
err_out:
	ch <- runScriptCompletion{
		stdoutFile:   "",
		stderrFile:   "",
		scriptStatus: err,
		err:          err}
}

func addNotes(ch chan<- error, sourceDir string, branch string,
	stdoutFile string, stderrFile string) {

	var err error = nil
	type notesOut struct {
		ns  string
		msg string
	}

	for _, note := range []notesOut{
		{ns: stdoutNotesRef, msg: stdoutFile},
		{ns: stderrNotesRef, msg: stderrFile}} {

		gitArgs := []string{"notes", "--ref", note.ns, "add",
			"--allow-empty", "-F", note.msg, "origin/" + branch}
		cmd := exec.Command("git", gitArgs...)
		cmd.Dir = sourceDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
		if err != nil {
			log.Printf("git %v failed: %v", gitArgs, err)
			goto err_out
		}
	}
err_out:
	ch <- err
}

// push captured stdout and stderr notes to the results repository.
func pushResults(ch chan<- error, sourceDir string,
	branch string, tag string, u *url.URL, pushSrcToRslts bool) {

	gitArgs := []string{"push", u.String(), allNotesGlob}
	if pushSrcToRslts {
		gitArgs = append(gitArgs, "HEAD:refs/heads/"+branch)
		if tag != "" {
			gitArgs = append(gitArgs, "refs/tags/"+tag)
		}
	}
	cmd := exec.Command("git", gitArgs...)
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
		log.Print("git rev-parse failed: %v\n", err)
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
		log.Print("git fetch failed: %v\n", err)
		goto err_out
	}
err_out:
	return err
}

func pollSource(ch chan<- error, sourceDir string, branch string,
	pollIntervalS uint64) {

	var err error = nil
	consecutiveFetchFail := 0
	const maxConsecutiveFetchFails = 10

	preFetchRev, err := pollGetRev(sourceDir, branch)
	if err != nil {
		log.Print("failed to get pre-fetch revision: %v\n", err)
		goto err_out
	}
	log.Printf("Entering poll loop awaiting new %s commits at %s\n",
		branch, preFetchRev)

	for {
		time.Sleep(time.Duration(time.Duration(pollIntervalS) * time.Second))

		err = pollFetch(sourceDir, branch)
		if err != nil {
			consecutiveFetchFail++
			if consecutiveFetchFail > maxConsecutiveFetchFails {
				break
			}
			log.Printf("Retrying; %d of maximum %d fetch failures\n",
				consecutiveFetchFail, maxConsecutiveFetchFails)
			continue
		}
		consecutiveFetchFail = 0

		postFetchRev, err := pollGetRev(sourceDir, branch)
		if err != nil {
			break
		}
		if preFetchRev != postFetchRev {
			log.Printf("detected branch %s change %s -> %s\n",
				branch, preFetchRev, postFetchRev)
			break
		}
	}
err_out:
	ch <- err
}

func transitionState(newState State, ls *loopState) {
	oldState := ls.state
	ls.state = newState

	_, exists := states[newState]
	if !exists {
		log.Fatalf("no states entry for %d\n")
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
func eventLoop(params *cliParams, workDir string) {

	sourceDir := path.Join(workDir, "source")

	// TODO reuse channels instead of creating one per activity
	cloneChan := make(chan error)
	verifyChan := make(chan verifyCompletion)
	pushLockChan := make(chan error)
	runScriptChan := make(chan runScriptCompletion)
	addNotesChan := make(chan error)
	pushResultsChan := make(chan error)
	cleanupChan := make(chan error)
	pollChan := make(chan error)

	var ls loopState
	transitionState(clone, &ls)
	go func() {
		cloneRepo(cloneChan, workDir, params.sourceUrl,
			params.sourceBranch, sourceDir)
	}()

	for {
		select {
		case cloneErr := <-cloneChan:
			if cloneErr != nil {
				log.Fatal(cloneErr)
			}
			log.Printf("clone completed successfully\n")

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
					pollSource(pollChan, sourceDir,
						params.sourceBranch,
						params.pollIntervalS)
				}()
				continue
			}
			ls.verifiedTag = verifyCmpl.tag
			log.Printf("verify completed successfully\n")

			transitionState(lock, &ls)
			go func() {
				pushLock(pushLockChan, sourceDir,
					params.sourceBranch, params.resultsUrl)
			}()

		case pushLockErr := <-pushLockChan:
			if pushLockErr != nil {
				transitionState(poll, &ls)
				go func() {
					pollSource(pollChan, sourceDir,
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
			if runScriptCmpl.scriptStatus == nil {
				log.Printf("test script completed successfully\n")
			} else {
				log.Printf("test script failed: %v\n",
					runScriptCmpl.scriptStatus)
			}

			transitionState(save, &ls)
			go func() {
				addNotes(addNotesChan, sourceDir,
					params.sourceBranch,
					runScriptCmpl.stdoutFile,
					runScriptCmpl.stderrFile)
			}()
		case addNotesErr := <-addNotesChan:
			if addNotesErr != nil {
				log.Fatal(addNotesErr)
			}
			log.Printf("git notes added successfully\n")

			transitionState(push, &ls)
			go func() {
				pushResults(pushResultsChan, sourceDir,
					params.sourceBranch, ls.verifiedTag,
					params.resultsUrl, params.pushSrcToRslts)
			}()
		case pushResultsErr := <-pushResultsChan:
			if pushResultsErr != nil {
				log.Fatal(pushResultsErr)
			}
			log.Printf("git push completed successfully\n")

			transitionState(cleanup, &ls)
			go func() {
				cleanupSource(cleanupChan, sourceDir)
			}()
		case cleanupErr := <-cleanupChan:
			if cleanupErr != nil {
				log.Fatal(cleanupErr)
			}
			log.Printf("cleanup completed successfully\n")

			transitionState(poll, &ls)
			go func() {
				pollSource(pollChan, sourceDir,
					params.sourceBranch,
					params.pollIntervalS)
			}()
		case pollErr := <-pollChan:
			if pollErr != nil {
				log.Fatal(pollErr)
			}
			log.Printf("poll / fetch loop returned success\n")

			transitionState(verify, &ls)
			go func() {
				verifyRepo(verifyChan, sourceDir,
					params.sourceBranch)
			}()
		case <-ls.transitionTimer.C:
			log.Fatalf("State %v transition timeout!", ls.state)
		}
	}
}

func main() {
	var sourceRawUrl string
	var resultsRawUrl string
	var err error

	params := new(cliParams)
	flag.Usage = usage
	flag.StringVar(&sourceRawUrl, "source-repo", "",
		"Git URL for the repository under test (required)")
	flag.StringVar(&params.sourceBranch, "source-branch", "master",
		"Git branch for the repository under test")
	flag.StringVar(&params.testScript, "test-script", "",
		"Test script path, relative to source-repo or absolute (required)")
	flag.StringVar(&resultsRawUrl, "results-repo", "",
		"Git URL to push test results to (optional)")
	// If the corresponding source branch is not pushed, then cloning the
	// results repository only (if different to source) may lead to
	// confusion due to the missing commits referenced by the notes.
	flag.BoolVar(&params.pushSrcToRslts, "push-source-to-results", true,
		"Push source-branch and any tag to results-repo, in addition to notes")
	flag.Uint64Var(&params.pollIntervalS, "poll-interval", 60,
		"While idle, poll source-repo for changes at this interval")
	flag.Parse()

	if sourceRawUrl == "" || params.testScript == "" {
		usage()
		return
	}

	params.sourceUrl, err = url.Parse(sourceRawUrl)
	if err != nil {
		log.Fatalf("failed to parse URL \"%s\": %v\n",
			sourceRawUrl, err)
	}

	if resultsRawUrl != "" {
		params.resultsUrl, err = url.Parse(resultsRawUrl)
		if err != nil {
			log.Fatalf("failed to parse URL \"%s\": %v\n",
				resultsRawUrl, err)
		}
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

	eventLoop(params, wdir)
}
