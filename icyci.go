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
	sourceUrl     *url.URL
	sourceBranch  string
	testScript    string
	resultsUrl    *url.URL
	resultsBranch string
}

type State int

const (
	uninit State = iota
	clone
	verify
	run
	save
	push
	cleanup
)

type stateDesc struct {
	desc    string
	timeout time.Duration
}

var states = map[State]stateDesc{
	uninit:  {"uninitialized", time.Duration(0)},
	clone:   {"clone source repository", time.Duration(10 * time.Minute)},
	verify:  {"verify branch HEAD", time.Duration(1 * time.Minute)},
	run:     {"run test", time.Duration(2 * time.Hour)},
	save:    {"save test output as git notes", time.Duration(1 * time.Minute)},
	push:    {"push test output notes", time.Duration(1 * time.Minute)},
	cleanup: {"cleanup test artifacts", time.Duration(1 * time.Minute)},
}

const stdoutNotesRef = "refs/notes/icyci.stdout"
const stderrNotesRef = "refs/notes/icyci.stderr"
const allNotesGlob = "refs/notes/*"

type cloneCompletion struct {
	err error
}

func cloneRepo(ch chan<- cloneCompletion, u *url.URL, branch string,
	targetDir string) {
	// TODO branch is not sanitized. Ignore submodules for now.
	if branch == "" {
		log.Fatal("empty branch name")
	}
	gitArgs := []string{"clone", "--no-checkout", "--single-branch",
		"--branch", branch, u.String(), targetDir}
	cmd := exec.Command("git", gitArgs...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Start()
	if err != nil {
		log.Printf("git failed to start: %v", err)
		goto err_out
	}

	err = cmd.Wait()
	if err != nil {
		log.Printf("git %v failed: %v", gitArgs, err)
		goto err_out
	}
err_out:
	ch <- cloneCompletion{err}
}

func verifyTag(branch string, tag string) error {
	log.Printf("GPG verifying tag %s at origin/%s", tag, branch)

	gitArgs := []string{"tag", "--verify", tag}
	cmd := exec.Command("git", gitArgs...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Start()
	if err != nil {
		return err
	}

	return cmd.Wait()
}

func verifyCommit(branch string) error {
	log.Printf("GPG verifying commit at origin/%s HEAD\n", branch)

	gitArgs := []string{"verify-commit", "origin/" + branch}
	cmd := exec.Command("git", gitArgs...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Start()
	if err != nil {
		return err
	}

	return cmd.Wait()
}

type verifyCompletion struct {
	err error
}

func verifyRepo(ch chan<- verifyCompletion, sourceDir string, branch string) {
	var describeOut bytes.Buffer
	cmd := exec.Command("git", "describe", "--tags", "--exact-match",
		"origin/"+branch)

	// check for a signed tag at HEAD
	err := os.Chdir(sourceDir)
	if err != nil {
		log.Printf("chdir failed: %v", err)
		goto err_out
	}

	cmd.Stdout = &describeOut
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		log.Printf("git failed to start: %v", err)
		goto err_out
	}

	err = cmd.Wait()
	if err == nil {
		// if there are >=2 tags at HEAD, git-describe outputs only one.
		// If both unsigned and signed tags exist, git-describe outputs
		// a signed tag.
		tag := string(bytes.TrimRight(describeOut.Bytes(), "\n"))
		err = verifyTag(branch, tag)
		if err != nil {
			log.Printf("GPG verification of tag %s at origin/%s failed",
				tag, branch)
			goto err_out
		}
	} else {
		// XXX should ignore merge commits?
		err = verifyCommit(branch)
		if err != nil {
			log.Printf("GPG verification of commit at origin/%s failed",
				branch)
			goto err_out
		}
	}
err_out:
	ch <- verifyCompletion{err}
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

	err := os.Chdir(sourceDir)
	if err != nil {
		log.Printf("chdir failed: %v", err)
		goto err_out
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		log.Printf("git failed to start: %v", err)
		goto err_out
	}

	err = cmd.Wait()
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

	err = os.Chdir(sourceDir)
	if err != nil {
		log.Printf("chdir failed: %v", err)
		goto err_out
	}

	cmd = exec.Command(testScript)
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
		stdoutFile: "",
		stderrFile: "",
		scriptStatus: err,
		err: err}
}

type notesOut struct {
	ns string
	msg  string
}

type addNotesCompletion struct {
	err error
}

func addNotes(ch chan<- addNotesCompletion, sourceDir string, branch string,
	stdoutFile string, stderrFile string) {

	err := os.Chdir(sourceDir)
	if err != nil {
		log.Printf("chdir failed: %v", err)
		goto err_out
	}

	for _, note := range []notesOut{
		{ns: stdoutNotesRef, msg: stdoutFile},
		{ns: stderrNotesRef, msg: stderrFile}} {

		gitArgs := []string{"notes", "--ref", note.ns, "append",
			"--allow-empty", "-F", note.msg, "origin/" + branch}
		cmd := exec.Command("git", gitArgs...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Start()
		if err != nil {
			log.Printf("git failed to start: %v", err)
			goto err_out
		}

		err = cmd.Wait()
		if err != nil {
			log.Printf("git %v failed: %v", gitArgs, err)
			goto err_out
		}
	}
err_out:
	ch <- addNotesCompletion{err}
}

type pushResultsCompletion struct {
	err error
}

// push captured stdout and stderr notes to the results repository.
// The corresponding source branch is *not* pushed, so cloning the results
// repository only (if different to source) may lead to confusion due to the
// missing commits referenced by the notes.
func pushResults(ch chan<- pushResultsCompletion, sourceDir string,
	branch string, u *url.URL) {
	cmd := exec.Command("git", "push", u.String(), allNotesGlob)

	err := os.Chdir(sourceDir)
	if err != nil {
		log.Printf("chdir failed: %v", err)
		goto err_out
	}

	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Start()
	if err != nil {
		log.Printf("git failed to start: %v", err)
		goto err_out
	}

	err = cmd.Wait()
	if err != nil {
		log.Printf("git failed: %v", err)
		goto err_out
	}
err_out:
	ch <- pushResultsCompletion{err}
}

type cleanupCompletion struct {
	err error
}

func cleanupSource(ch chan<- cleanupCompletion, sourceDir string) {
	cmd := exec.Command("git", "clean", "-fxd")

	err := os.Chdir(sourceDir)
	if err != nil {
		log.Printf("chdir failed: %v", err)
		goto err_out
	}

	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Start()
	if err != nil {
		log.Printf("git failed to start: %v", err)
		goto err_out
	}

	err = cmd.Wait()
	if err != nil {
		log.Printf("git failed: %v", err)
		goto err_out
	}
err_out:
	ch <- cleanupCompletion{err}
}

func transitionState(newState State, curState *State,
	stateTransTimer *time.Timer) {
	log.Printf("transitioning from state %d: %s -> %d: %s\n",
		*curState, states[*curState].desc,
		newState, states[newState].desc)

	*curState = newState

	if !stateTransTimer.Stop() {
		<-stateTransTimer.C
	}
	stateTransTimer.Reset(states[newState].timeout)
}

// event loop to track state of clone, verify, test, push progress
func eventLoop(params *cliParams, workDir string) {

	sourceDir := path.Join(workDir, "source")

	// TODO reuse channels instead of creating one per activity
	cloneChan := make(chan cloneCompletion)
	verifyChan := make(chan verifyCompletion)
	runScriptChan := make(chan runScriptCompletion)
	addNotesChan := make(chan addNotesCompletion)
	pushResultsChan := make(chan pushResultsCompletion)
	cleanupChan := make(chan cleanupCompletion)

	state := uninit
	stateTransTimer := time.NewTimer(time.Duration(0))
	transitionState(clone, &state, stateTransTimer)
	go func() {
		cloneRepo(cloneChan, params.sourceUrl, params.sourceBranch,
			sourceDir)
	}()

	for {
		select {
		case cloneCmpl := <-cloneChan:
			if cloneCmpl.err != nil {
				log.Fatal(cloneCmpl.err)
			}
			log.Printf("clone completed successfully\n")

			transitionState(verify, &state, stateTransTimer)
			go func() {
				verifyRepo(verifyChan, sourceDir,
					params.sourceBranch)
			}()
		case verifyCmpl := <-verifyChan:
			if verifyCmpl.err != nil {
				log.Fatal(verifyCmpl.err)
			}
			log.Printf("verify completed successfully\n")

			transitionState(run, &state, stateTransTimer)
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

			transitionState(save, &state, stateTransTimer)
			go func() {
				addNotes(addNotesChan, sourceDir,
					params.sourceBranch,
					runScriptCmpl.stdoutFile,
					runScriptCmpl.stderrFile)
			}()
		case addNotesCmpl := <-addNotesChan:
			if addNotesCmpl.err != nil {
				log.Fatal(addNotesCmpl.err)
			}
			log.Printf("git notes completed successfully\n")

			transitionState(push, &state, stateTransTimer)
			go func() {
				pushResults(pushResultsChan, sourceDir,
					params.sourceBranch, params.resultsUrl)
			}()
		case pushResultsCmpl := <-pushResultsChan:
			if pushResultsCmpl.err != nil {
				log.Fatal(pushResultsCmpl.err)
			}
			log.Printf("git push completed successfully\n")

			transitionState(cleanup, &state, stateTransTimer)
			go func() {
				cleanupSource(cleanupChan, sourceDir)
			}()
		case cleanupCmpl := <-cleanupChan:
			if cleanupCmpl.err != nil {
				log.Fatal(cleanupCmpl.err)
			}
			log.Printf("cleanup completed successfully\n")

			// all done. TODO: poll for updates
			return
		case <-stateTransTimer.C:
			log.Fatalf("State %v transition timeout!", state)
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
