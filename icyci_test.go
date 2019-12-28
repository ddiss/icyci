// SPDX-License-Identifier: AGPL-3.0-only
//
// Copyright (C) 2019 SUSE LLC

package main

import (
	"bytes"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"sync"
	"testing"
	"time"
)

const (
	userName  = "icyCI test"
	userEmail = "icyci@example.com"
)

func gpgInit(t *testing.T, tdir string) {
	// GNUPGHOME for key import and verification
	gpgDir := path.Join(tdir, "gpg")

	// export HOME and GNUPGHOME to ensure that our custom git + gpg configs
	// are picked up for all git operations.
	os.Setenv("HOME", tdir)
	os.Setenv("GNUPGHOME", gpgDir)
	os.Setenv("GIT_PAGER", "")

	err := os.MkdirAll(gpgDir, 0700)
	if err != nil {
		t.Fatal(err)
	}

	batchScript := `
		%echo starting keygen
		Key-Type: default
		Subkey-Type: default
		Name-Real: ` + userName + `
		Name-Comment: test user
		Name-Email: ` + userEmail + `
		Expire-Date: 1d
		%no-protection
		%transient-key
		%commit
		%echo done`

	// create a tempdir to use as GNUPGHOME for key import and verification
	batchFile := path.Join(gpgDir, "/batch_script.txt")

	err = ioutil.WriteFile(batchFile,
		[]byte(batchScript), os.FileMode(0644))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("gpg", "--homedir", gpgDir, "--gen-key", "--batch",
		batchFile)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("created GPG keypair at %s with key id %s", gpgDir, userEmail)
}

func gitReposInit(t *testing.T, gitHomeDir string, sdir string, rdir string) {

	gitCfg := path.Join(gitHomeDir, ".gitconfig")
	err := ioutil.WriteFile(gitCfg,
		[]byte(`[user]
			name = `+userName+`
		        email = `+userEmail+`
			signingKey = <`+userEmail+`>`),
		os.FileMode(0644))
	if err != nil {
		t.Fatal(err)
	}

	dupFilter := make(map[string]bool)
	for _, dir := range []string{sdir, rdir} {
		if dupFilter[dir] {
			continue
		}
		dupFilter[dir] = true
		cmd := exec.Command("git", "init", dir)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Run()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func fileWriteCommit(t *testing.T, sdir string, sfile string,
	echoStr string, sign bool) string {
	srcPath := path.Join(sdir, sfile)
	err := ioutil.WriteFile(srcPath,
		[]byte(`#!/bin/bash
			echo "`+echoStr+`"`),
		os.FileMode(0755))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "add", srcPath)
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	gitCmd := []string{"commit", "-a"}
	if sign {
		gitCmd = append(gitCmd, "-S", "-m", "signed source commit")
	} else {
		gitCmd = append(gitCmd, "-m", "unsigned source commit")
	}

	cmd = exec.Command("git", gitCmd...)
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	var revParseOut bytes.Buffer
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = sdir
	cmd.Stdout = &revParseOut
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	curRev := string(bytes.TrimRight(revParseOut.Bytes(), "\n"))
	if sign {
		t.Logf("%s: signed commit %s script: echo \"%s\"\n",
			curRev, sfile, echoStr)
	} else {
		t.Logf("%s: unsigned commit %s script: echo \"%s\"\n",
			curRev, sfile, echoStr)
	}

	return curRev
}

func fileWriteSignedCommit(t *testing.T, sdir string, sfile string,
	echoStr string) string {
	return fileWriteCommit(t, sdir, sfile, echoStr, true)
}

func fileWriteUnsignedCommit(t *testing.T, sdir string, sfile string,
	echoStr string) string {
	return fileWriteCommit(t, sdir, sfile, echoStr, false)
}

func waitNotes(t *testing.T, repoDir string, notesRef string, srcRef string,
	notesChan chan<- bytes.Buffer) {

	for {
		var notesOut bytes.Buffer

		t.Logf("updating remotes")
		cmd := exec.Command("git", "remote", "update")
		cmd.Dir = repoDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Run()
		if err != nil {
			t.Fatal(err)
		}

		t.Logf("checking notes at %s", srcRef)
		cmd = exec.Command("git", "notes", "--ref="+notesRef, "show",
			"--", srcRef)
		cmd.Dir = repoDir
		cmd.Stdout = &notesOut
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err == nil {
			// notes arrived, notify
			t.Logf("notes ready!")
			notesChan <- notesOut
			return
		}

		time.Sleep(time.Second * 1)
	}
}

// Simple test for (mostly) default case:
// - source and results are separate git repos
// - test script is in the source repo
// - single icyCI instance trusting only one key
func TestSeparateSrcRslt(t *testing.T) {

	tdir, err := ioutil.TempDir("", "icyci-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tdir)

	gpgInit(t, tdir)

	sdir := path.Join(tdir, "test_src")
	rdir := path.Join(tdir, "test_rslt")
	gitReposInit(t, tdir, sdir, rdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	fileWriteSignedCommit(t, sdir, "src_test.sh",
		"this has been run by icyci")

	surl, err := url.Parse(sdir)
	rurl, err := url.Parse(rdir)
	params := cliParams{
		sourceUrl:      surl,
		sourceBranch:   "mybranch",
		testScript:     "./src_test.sh",
		resultsUrl:     rurl,
		pushSrcToRslts: false,
		pollIntervalS:  60,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	evExitChan := make(chan int)
	go func() {
		eventLoop(&params, tdir, evExitChan)
		wg.Done()
	}()

	// clone source and add results repo as a remote
	cloneDir := path.Join(tdir, "test_clone_both")

	cmd = exec.Command("git", "clone", sdir, cloneDir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("git", "remote", "add", "results", rdir)
	cmd.Dir = cloneDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("git", "config", "--add", "remote.results.fetch",
		"refs/notes/*:refs/notes/*")
	cmd.Dir = cloneDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	// wait for the results git-notes to arrive from the icyCI event loop
	notesChan := make(chan bytes.Buffer)
	go func() {
		waitNotes(t, cloneDir, stdoutNotesRef, "HEAD", notesChan)
	}()

	notesWaitTimer := time.NewTimer(time.Second * 10)
	for {
		select {
		case notes := <-notesChan:
			snotes := string(bytes.TrimRight(notes.Bytes(), "\n"))
			if snotes != "this has been run by icyci" {
				t.Fatalf("%s does not match expected\n", snotes)
			}
			// tell icyCI eventLoop to end
			evExitChan <- 1
			wg.Wait()
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
			return
		case <-notesWaitTimer.C:
			t.Fatal("timeout while waiting for icyCI notes\n")
		}
	}
}

// - source and results are same git repos
// - single icyCI instance trusting only one key
// - move source head forward
// - wait for new head to be tested successfully
// - loop over last two items "maxCommitI" times
func TestNewHeadSameSrcRslt(t *testing.T) {
	// commitI tracks the number of commits for which we should expect a
	// corresponding results note entry.
	var commitI int = 0
	var curCommit string
	const maxCommitI int = 3

	tdir, err := ioutil.TempDir("", "icyci-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tdir)

	gpgInit(t, tdir)

	sdir := path.Join(tdir, "test_src_and_rslt")
	rdir := sdir
	gitReposInit(t, tdir, sdir, rdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	curCommit = fileWriteSignedCommit(t, sdir, "src_test.sh",
		"commitI: "+strconv.Itoa(commitI))
	commitI++

	surl, err := url.Parse(sdir)
	rurl, err := url.Parse(rdir)
	params := cliParams{
		sourceUrl:      surl,
		sourceBranch:   "mybranch",
		testScript:     "./src_test.sh",
		resultsUrl:     rurl,
		pushSrcToRslts: false,
		pollIntervalS:  1, // minimal
	}

	var wg sync.WaitGroup
	wg.Add(1)
	evExitChan := make(chan int)
	go func() {
		eventLoop(&params, tdir, evExitChan)
		wg.Done()
	}()

	// clone source and add results repo as a remote
	cloneDir := path.Join(tdir, "test_clone_both")

	cmd = exec.Command("git", "clone", "--config",
		"remote.origin.fetch=refs/notes/*:refs/notes/*", sdir, cloneDir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	// wait for the results git-notes to arrive from the icyCI event loop
	notesChan := make(chan bytes.Buffer)
	go func() {
		waitNotes(t, cloneDir, stdoutNotesRef, curCommit, notesChan)
	}()

	notesWaitTimer := time.NewTimer(time.Second * 10)
	for {
		select {
		case notes := <-notesChan:
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
			snotes := string(bytes.TrimRight(notes.Bytes(), "\n"))
			if snotes != "commitI: "+strconv.Itoa(commitI-1) {
				t.Fatalf("%s does not match expected\n", snotes)
			}
			if commitI == maxCommitI {
				// Finished, tell icyCI eventLoop to end
				evExitChan <- 1
				wg.Wait()
				return
			}
			curCommit = fileWriteSignedCommit(
				t, sdir, "src_test.sh",
				"commitI: "+strconv.Itoa(commitI))
			commitI++
			go func() {
				waitNotes(t, cloneDir, stdoutNotesRef,
					curCommit, notesChan)
			}()
			notesWaitTimer.Reset(time.Second * 10)

		case <-notesWaitTimer.C:
			t.Fatal("timeout while waiting for icyCI notes\n")
		}
	}
}

// - single icyCI instance trusting only one key
// - stop instance
// - move source head forward
// - start new instance
func TestNewHeadWhileStopped(t *testing.T) {
	// commitI tracks the number of commits for which we should expect a
	// corresponding results note entry.
	var commitI int = 0
	var curCommit string
	const maxCommitI int = 3

	tdir, err := ioutil.TempDir("", "icyci-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tdir)

	gpgInit(t, tdir)

	sdir := path.Join(tdir, "test_src_and_rslt")
	rdir := sdir
	gitReposInit(t, tdir, sdir, rdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	curCommit = fileWriteSignedCommit(t, sdir, "src_test.sh",
		"commitI: "+strconv.Itoa(commitI))
	commitI++

	surl, err := url.Parse(sdir)
	rurl, err := url.Parse(rdir)
	params := cliParams{
		sourceUrl:      surl,
		sourceBranch:   "mybranch",
		testScript:     "./src_test.sh",
		resultsUrl:     rurl,
		pushSrcToRslts: false,
		pollIntervalS:  1, // minimal
	}

	var wg sync.WaitGroup
	wg.Add(1)
	evExitChan := make(chan int)
	go func() {
		t.Log("starting icyCI eventLoop")
		eventLoop(&params, tdir, evExitChan)
		wg.Done()
	}()

	// clone source and add results repo as a remote
	cloneDir := path.Join(tdir, "test_clone_both")

	cmd = exec.Command("git", "clone", "--config",
		"remote.origin.fetch=refs/notes/*:refs/notes/*", sdir, cloneDir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	// wait for the results git-notes to arrive from the icyCI event loop
	notesChan := make(chan bytes.Buffer)
	go func() {
		waitNotes(t, cloneDir, stdoutNotesRef, curCommit, notesChan)
	}()

	notesWaitTimer := time.NewTimer(time.Second * 10)
	for {
		select {
		case notes := <-notesChan:
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
			snotes := string(bytes.TrimRight(notes.Bytes(), "\n"))
			if snotes != "commitI: "+strconv.Itoa(commitI-1) {
				t.Fatalf("%s does not match expected\n", snotes)
			}

			t.Log("telling icyCI eventLoop to end")
			evExitChan <- 1
			wg.Wait()

			if commitI >= maxCommitI {
				return // all done
			}
			curCommit = fileWriteSignedCommit(
				t, sdir, "src_test.sh",
				"commitI: "+strconv.Itoa(commitI))
			commitI++
			curCommit = fileWriteSignedCommit(
				t, sdir, "src_test.sh",
				"commitI: "+strconv.Itoa(commitI))
			commitI++

			wg.Add(1)
			go func() {
				// we're reusing icyci's tmp dir, so need to
				// explicitly delete the "source" working dir
				os.RemoveAll(path.Join(tdir, "source"))
				t.Log("starting icyCI eventLoop")
				eventLoop(&params, tdir, evExitChan)
				wg.Done()
			}()
			go func() {
				waitNotes(t, cloneDir, stdoutNotesRef,
					curCommit, notesChan)
			}()
			notesWaitTimer.Reset(time.Second * 10)

		case <-notesWaitTimer.C:
			t.Fatal("timeout while waiting for icyCI notes\n")
		}
	}
}

type logParser struct {
	T      *testing.T
	needle []byte
	ch     chan<- bool
}

// XXX this assumes that the grepped string will be carried in its entirity in a
// Write(p) call. It probably makes sense to buffer full lines before compare.
func (lp *logParser) Write(p []byte) (int, error) {
	l := len(p)
	if l == 0 {
		return 0, nil
	}

	lp.T.Logf("parsing icyci log msg: %s", string(p))

	if p[l-1] != '\n' {
		lp.T.Logf("parsing log msg without line end - grep may fail!")
	}

	if bytes.Contains(p, lp.needle) {
		lp.ch <- true
	}
	return l, nil
}

// - single icyCI instance trusting only one key
// - stop instance
// - start new instance
// - check for lock failure by scraping logs
// - move forward head and ensure new commit is tested
func TestStopStart(t *testing.T) {
	// commitI tracks the number of commits for which we should expect a
	// corresponding results note entry.
	var commitI int = 0
	var curCommit string
	const maxCommitI int = 3

	tdir, err := ioutil.TempDir("", "icyci-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tdir)

	gpgInit(t, tdir)

	sdir := path.Join(tdir, "test_src_and_rslt")
	rdir := sdir
	gitReposInit(t, tdir, sdir, rdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	curCommit = fileWriteSignedCommit(t, sdir, "src_test.sh",
		"commitI: "+strconv.Itoa(commitI))
	commitI++

	surl, err := url.Parse(sdir)
	rurl, err := url.Parse(rdir)
	params := cliParams{
		sourceUrl:      surl,
		sourceBranch:   "mybranch",
		testScript:     "./src_test.sh",
		resultsUrl:     rurl,
		pushSrcToRslts: false,
		pollIntervalS:  1, // minimal
	}

	var wg sync.WaitGroup
	wg.Add(1)
	evExitChan := make(chan int)
	go func() {
		t.Log("starting icyCI eventLoop")
		eventLoop(&params, tdir, evExitChan)
		wg.Done()
	}()

	// clone source and add results repo as a remote
	cloneDir := path.Join(tdir, "test_clone_both")

	cmd = exec.Command("git", "clone", "--config",
		"remote.origin.fetch=refs/notes/*:refs/notes/*", sdir, cloneDir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	// wait for the results git-notes to arrive from the icyCI event loop
	notesChan := make(chan bytes.Buffer)
	go func() {
		waitNotes(t, cloneDir, stdoutNotesRef, curCommit, notesChan)
	}()

	grepChan := make(chan bool)
	notesWaitTimer := time.NewTimer(time.Second * 10)
	for {
		select {
		case notes := <-notesChan:
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
			snotes := string(bytes.TrimRight(notes.Bytes(), "\n"))
			if snotes != "commitI: "+strconv.Itoa(commitI-1) {
				t.Fatalf("%s does not match expected\n", snotes)
			}

			t.Log("telling icyCI eventLoop to end")
			evExitChan <- 1
			wg.Wait()

			if commitI >= maxCommitI {
				return // all done
			}

			lp := logParser{
				T:      t,
				needle: []byte("failed to add git notes lock"),
				ch:     grepChan,
			}
			log.SetOutput(&lp)
			t.Logf("parsing icyCI log for: %s", string(lp.needle))

			wg.Add(1)
			go func() {
				// we're reusing icyci's tmp dir, so need to
				// explicitly delete the "source" working dir
				os.RemoveAll(path.Join(tdir, "source"))
				t.Log("starting icyCI eventLoop")
				eventLoop(&params, tdir, evExitChan)
				wg.Done()
			}()
			notesWaitTimer.Reset(time.Second * 10)
		case <-grepChan:
			// restore log
			log.SetOutput(os.Stderr)

			curCommit = fileWriteSignedCommit(
				t, sdir, "src_test.sh",
				"commitI: "+strconv.Itoa(commitI))
			commitI++

			go func() {
				waitNotes(t, cloneDir, stdoutNotesRef,
					curCommit, notesChan)
			}()
			notesWaitTimer.Reset(time.Second * 10)
		case <-notesWaitTimer.C:
			t.Fatal("timeout while waiting for icyCI notes\n")
		}
	}
}

// - source HEAD isn't signed, but a corresponding signed tag is
// FIXME icyci currently only polls for heads, so the signed tag needs to be
// pushed before the new head
func TestSignedTagUnsignedCommit(t *testing.T) {
	// commitI tracks the number of commits for which we should expect a
	// corresponding results note entry.
	var commitI int = 0
	var curCommit string
	const maxCommitI int = 3

	tdir, err := ioutil.TempDir("", "icyci-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tdir)

	gpgInit(t, tdir)

	sdir := path.Join(tdir, "test_src_and_rslt")
	rdir := sdir
	gitReposInit(t, tdir, sdir, rdir)

	// commit from clone so that we can push the tag before the new head
	cloneDir := path.Join(tdir, "test_clone_both")

	cmd := exec.Command("git", "clone", "--config",
		"remote.origin.fetch=refs/notes/*:refs/notes/*", sdir, cloneDir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = cloneDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	curCommit = fileWriteUnsignedCommit(t, cloneDir, "src_test.sh",
		"commitI: "+strconv.Itoa(commitI))
	commitI++

	tagName := "mytag" + strconv.Itoa(commitI)
	cmd = exec.Command("git", "tag", "-s", "-m", "signed tag", tagName)
	cmd.Dir = cloneDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("git", "push", sdir, tagName, "mybranch:mybranch")
	cmd.Dir = cloneDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	surl, err := url.Parse(sdir)
	rurl, err := url.Parse(rdir)
	params := cliParams{
		sourceUrl:      surl,
		sourceBranch:   "mybranch",
		testScript:     "./src_test.sh",
		resultsUrl:     rurl,
		pushSrcToRslts: false,
		pollIntervalS:  1, // minimal
	}

	var wg sync.WaitGroup
	wg.Add(1)
	evExitChan := make(chan int)
	go func() {
		eventLoop(&params, tdir, evExitChan)
		wg.Done()
	}()

	// wait for the results git-notes to arrive from the icyCI event loop
	notesChan := make(chan bytes.Buffer)
	go func() {
		waitNotes(t, cloneDir, stdoutNotesRef, curCommit, notesChan)
	}()

	notesWaitTimer := time.NewTimer(time.Second * 10)
	for {
		select {
		case notes := <-notesChan:
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
			snotes := string(bytes.TrimRight(notes.Bytes(), "\n"))
			if snotes != "commitI: "+strconv.Itoa(commitI-1) {
				t.Fatalf("%s does not match expected\n", snotes)
			}
			if commitI == maxCommitI {
				// Finished, tell icyCI eventLoop to end
				evExitChan <- 1
				wg.Wait()
				return
			}
			curCommit = fileWriteUnsignedCommit(
				t, cloneDir, "src_test.sh",
				"commitI: "+strconv.Itoa(commitI))
			commitI++

			tagName = "mytag" + strconv.Itoa(commitI)
			cmd = exec.Command("git", "tag", "-s",
				"-m", "signed tag", tagName)
			cmd.Dir = cloneDir
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			err = cmd.Run()
			if err != nil {
				t.Fatal(err)
			}

			cmd = exec.Command("git", "push", sdir, tagName,
				"mybranch:mybranch")
			cmd.Dir = cloneDir
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			err = cmd.Run()
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				waitNotes(t, cloneDir, stdoutNotesRef,
					curCommit, notesChan)
			}()
			notesWaitTimer.Reset(time.Second * 10)

		case <-notesWaitTimer.C:
			t.Fatal("timeout while waiting for icyCI notes\n")
		}
	}
}
