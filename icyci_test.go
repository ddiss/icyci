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

func gitReposInit(t *testing.T, gitHomeDir string, repoDirs ...string) {

	gitCfg := path.Join(gitHomeDir, ".gitconfig")
	err := ioutil.WriteFile(gitCfg,
		[]byte(`[user]
			name = `+userName+`
		        email = `+userEmail+`
			signingKey = <`+userEmail+`>
			[init]
			defaultBranch = main`),
		os.FileMode(0644))
	if err != nil {
		t.Fatal(err)
	}

	dupFilter := make(map[string]bool)
	for _, dir := range repoDirs {
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

func fileWriteCommit(t *testing.T, sdir string, sfiles map[string]string,
	sign bool) string {
	for sfile, script := range sfiles {
		srcPath := path.Join(sdir, sfile)
		err := ioutil.WriteFile(srcPath,
			[]byte("#!/bin/bash\n"+script),
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
	}

	gitCmd := []string{"commit"}
	if sign {
		gitCmd = append(gitCmd, "-S", "-m", "signed source commit")
	} else {
		gitCmd = append(gitCmd, "-m", "unsigned source commit")
	}

	cmd := exec.Command("git", gitCmd...)
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
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
		t.Logf("%s: signed commit: %v\n", curRev, sfiles)
	} else {
		t.Logf("%s: unsigned commit: %v\n", curRev, sfiles)
	}

	return curRev
}

func fileWriteSignedCommit(t *testing.T, sdir string, sfile string,
	script string) string {
	return fileWriteCommit(t, sdir, map[string]string{sfile: script}, true)
}

func fileWriteUnsignedCommit(t *testing.T, sdir string, sfile string,
	script string) string {
	return fileWriteCommit(t, sdir, map[string]string{sfile: script}, false)
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
		`echo "this has been run by icyci"`)

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
	gitReposInit(t, tdir, sdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	curCommit = fileWriteSignedCommit(t, sdir, "src_test.sh",
		`echo "commitI: `+strconv.Itoa(commitI)+`"`)
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
				`echo "commitI: `+strconv.Itoa(commitI)+`"`)
			commitI++
			go func() {
				waitNotes(t, cloneDir, stdoutNotesRef,
					curCommit, notesChan)
			}()
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
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
	gitReposInit(t, tdir, sdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	curCommit = fileWriteSignedCommit(t, sdir, "src_test.sh",
		`echo "commitI: `+strconv.Itoa(commitI)+`"`)
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
				`echo "commitI: `+strconv.Itoa(commitI)+`"`)
			commitI++
			curCommit = fileWriteSignedCommit(
				t, sdir, "src_test.sh",
				`echo "commitI: `+strconv.Itoa(commitI)+`"`)
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
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
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
	gitReposInit(t, tdir, sdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	curCommit = fileWriteSignedCommit(t, sdir, "src_test.sh",
		`echo "commitI: `+strconv.Itoa(commitI)+`"`)
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
				needle: []byte("couldn't add git notes lock"),
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
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
			notesWaitTimer.Reset(time.Second * 10)
		case <-grepChan:
			// restore log
			log.SetOutput(os.Stderr)

			curCommit = fileWriteSignedCommit(
				t, sdir, "src_test.sh",
				`echo "commitI: `+strconv.Itoa(commitI)+`"`)
			commitI++

			go func() {
				waitNotes(t, cloneDir, stdoutNotesRef,
					curCommit, notesChan)
			}()
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
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
	gitReposInit(t, tdir, sdir)

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
		`echo "commitI: `+strconv.Itoa(commitI)+`"`)
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
				`echo "commitI: `+strconv.Itoa(commitI)+`"`)
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
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
			notesWaitTimer.Reset(time.Second * 10)

		case <-notesWaitTimer.C:
			t.Fatal("timeout while waiting for icyCI notes\n")
		}
	}
}

// - first commit is signed, then alternate between signed and unsigned
func TestMixUnsignedSigned(t *testing.T) {
	var commitI int = 0
	var curCommit string
	const maxCommitI int = 4

	tdir, err := ioutil.TempDir("", "icyci-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tdir)

	gpgInit(t, tdir)

	sdir := path.Join(tdir, "test_src_and_rslt")
	rdir := sdir
	gitReposInit(t, tdir, sdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	curCommit = fileWriteSignedCommit(t, sdir, "src_test.sh",
		`echo "commitI: `+strconv.Itoa(commitI)+`"`)
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

			if commitI >= maxCommitI {
				// Finished, tell icyCI eventLoop to end
				evExitChan <- 1
				wg.Wait()
				return
			}

			lp := logParser{
				T:      t,
				needle: []byte("GPG verification of commit at origin/mybranch failed"),
				ch:     grepChan,
			}
			log.SetOutput(&lp)
			t.Logf("parsing icyCI log for: %s", string(lp.needle))

			curCommit = fileWriteUnsignedCommit(
				t, sdir, "src_test.sh",
				`echo "commitI: `+strconv.Itoa(commitI)+`"`)
			commitI++

			notesWaitTimer.Reset(time.Second * 10)
		case <-grepChan:
			// restore log
			log.SetOutput(os.Stderr)

			curCommit = fileWriteSignedCommit(
				t, sdir, "src_test.sh",
				`echo "commitI: `+strconv.Itoa(commitI)+`"`)
			commitI++

			go func() {
				waitNotes(t, cloneDir, stdoutNotesRef,
					curCommit, notesChan)
			}()
			if !notesWaitTimer.Stop() {
				<-notesWaitTimer.C
			}
			notesWaitTimer.Reset(time.Second * 10)
		case <-notesWaitTimer.C:
			t.Fatal("timeout while waiting for icyCI notes\n")
		}
	}
}

func waitSpinlk(t *testing.T, spinlkPath string, spinlkChan chan<- bool) {

	t.Logf("waiting for spinlk file at: %s", spinlkPath)
	for {
		_, err := os.Stat(spinlkPath)
		if err == nil {
			t.Logf("spinlk file present at: %s", spinlkPath)
			spinlkChan <- true
			return
		}

		time.Sleep(time.Second * 1)
	}
}

type instanceState struct {
	id         string
	tmpDir     string
	spinlk     string
	spinlkChan chan bool
	wg         sync.WaitGroup
	evExitChan chan int
	commit     string
	commitI    int
	notesChan  chan bytes.Buffer
	gotNotes   bool
	params     cliParams
}

type commitState struct {
	nextCommitI int
	m           map[int]string
}

func handleSpinlk(t *testing.T, sdir string, cloneDir string,
	iWin *instanceState, iLost *instanceState,
	cs *commitState) {

	// iWin instance won the race to push a lock and start testScript.
	// It's is blocked awaiting spinlk removal
	iWin.commitI = cs.nextCommitI - 1
	iWin.commit = cs.m[iWin.commitI]
	_, err := os.Stat(iLost.spinlk)
	if err == nil {
		t.Logf("%s spinlk with %s present, proceeding",
			iWin.spinlk, iLost.spinlk)
		if iWin.commitI != iLost.commitI+1 {
			t.Fatalf("iWin.commitI %d != iLost.commitI+1 %d",
				iWin.commitI, iLost.commitI+1)
		}

		// iLost is also blocked on a spinlk from a prior commit.
		// Remove both spinlocks and wait for notes
		os.Remove(iWin.spinlk)
		os.Remove(iLost.spinlk)
		go func() {
			// sync - remote update can't be run concurrently in the
			// same cloneDir
			waitNotes(t, cloneDir, stdoutNotesRef,
				iWin.commit, iWin.notesChan)
			waitNotes(t, cloneDir, stdoutNotesRef,
				iLost.commit, iLost.notesChan)
		}()
		return
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	// iLost is still polling, so a new commit should be picked up by it
	fm := map[string]string{
		iWin.params.testScript: `echo "` + iWin.id + `: commitI: ` +
			strconv.Itoa(cs.nextCommitI) + `"
			touch ` + iWin.spinlk + `
			while [ -f ` + iWin.spinlk + ` ]; do sleep 1; done`,
		iLost.params.testScript: `echo "` + iLost.id + `: commitI: ` +
			strconv.Itoa(cs.nextCommitI) + `"
			touch ` + iLost.spinlk + `
			while [ -f ` + iLost.spinlk + ` ]; do sleep 1; done`}
	cs.m[cs.nextCommitI] = fileWriteCommit(t, sdir, fm, true)
	cs.nextCommitI++
}

func checkResults(t *testing.T, repoDir string, commit string, script string) {
	var notesOut bytes.Buffer

	cmd := exec.Command("git", "notes", "--ref="+passedNotesRef, "show",
		"--", commit)
	cmd.Dir = repoDir
	cmd.Stdout = &notesOut
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
	snotes := string(bytes.TrimRight(notesOut.Bytes(), "\n"))
	if snotes != script+" completed successfully" {
		t.Fatalf("%s does not match expected\n", snotes)
	}
	t.Logf("%s matches expected: %s", passedNotesRef, snotes)
}

// - test multiple concurrent icyCI instances running against the same source
// - instances use "spinlk" files for synchronisation during testing
// - commit changes and start both (i1 and i2) instances
// - one instance picks up the new commit and runs its testScript
// - the other instance remains in the icyci git fetch loop
// - testScript creates a spinlk file and waits for its removal
// - waitSpinlk() detects the instance blocked by spinlk
// - commit another change, to be picked up by the other instance in the git
//   fetch loop
// - wait for the second spinlk file to appear
// - remove both spinlk files and wait for notes
func TestMultiInstance(t *testing.T) {
	cs := commitState{
		nextCommitI: 0,
		m:           make(map[int]string),
	}

	tdir, err := ioutil.TempDir("", "icyci-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tdir)

	gpgInit(t, tdir)

	sdir := path.Join(tdir, "test_src_and_rslt")
	rdir := sdir
	gitReposInit(t, tdir, sdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	surl, err := url.Parse(sdir)
	if err != nil {
		t.Fatal(err)
	}
	rurl, err := url.Parse(rdir)
	if err != nil {
		t.Fatal(err)
	}

	i1 := instanceState{id: "i1"}
	i2 := instanceState{id: "i2"}
	for _, i := range []*instanceState{&i1, &i2} {
		i.tmpDir = path.Join(tdir, i.id)
		err = os.Mkdir(i.tmpDir, 0755)
		if err != nil {
			t.Fatal(err)
		}
		// spinlk files block instances while running the test script
		i.spinlk = path.Join(i.tmpDir, i.id+"_spinlk")

		// each instance uses a separate testScript, committed below...
		i.params = cliParams{
			sourceUrl:      surl,
			sourceBranch:   "mybranch",
			testScript:     "./" + i.id + "_test.sh",
			resultsUrl:     rurl,
			pushSrcToRslts: false,
			pollIntervalS:  1,
		}

		i.evExitChan = make(chan int)
		i.spinlkChan = make(chan bool)
		i.notesChan = make(chan bytes.Buffer)
	}

	fm := map[string]string{
		"i1_test.sh": `echo "i1: commitI: ` + strconv.Itoa(cs.nextCommitI) + `"
			touch ` + i1.spinlk + `
			while [ -f ` + i1.spinlk + ` ]; do sleep 1; done`,
		"i2_test.sh": `echo "i2: commitI: ` + strconv.Itoa(cs.nextCommitI) + `"
			touch ` + i2.spinlk + `
			while [ -f ` + i2.spinlk + ` ]; do sleep 1; done`}
	cs.m[cs.nextCommitI] = fileWriteCommit(t, sdir, fm, true)
	cs.nextCommitI++

	i1.wg.Add(1)
	go func() {
		t.Log("starting i1 icyCI eventLoop")
		eventLoop(&i1.params, i1.tmpDir, i1.evExitChan)
		i1.wg.Done()
	}()
	i2.wg.Add(1)
	go func() {
		t.Log("starting i2 icyCI eventLoop")
		eventLoop(&i2.params, i2.tmpDir, i2.evExitChan)
		i2.wg.Done()
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

	// wait for either instance to create their spinlk file, which
	// signifies that they've won the race to obtain the icyci lock.
	go func() {
		waitSpinlk(t, i1.spinlk, i1.spinlkChan)
	}()
	go func() {
		waitSpinlk(t, i2.spinlk, i2.spinlkChan)
	}()

	waitTimer := time.NewTimer(time.Second * 10)
	for {
		select {
		case <-i1.spinlkChan:
			if !waitTimer.Stop() {
				<-waitTimer.C
			}
			waitTimer.Reset(time.Second * 10)
			handleSpinlk(t, sdir, cloneDir, &i1, &i2, &cs)
		case <-i2.spinlkChan:
			if !waitTimer.Stop() {
				<-waitTimer.C
			}
			waitTimer.Reset(time.Second * 10)
			handleSpinlk(t, sdir, cloneDir, &i2, &i1, &cs)
		case n1 := <-i1.notesChan:
			t.Log("i1 complete")
			if !waitTimer.Stop() {
				<-waitTimer.C
			}
			waitTimer.Reset(time.Second * 10)
			snotes := string(bytes.TrimRight(n1.Bytes(), "\n"))
			expected := "i1: commitI: " + strconv.Itoa(i1.commitI)
			if snotes != expected {
				t.Fatalf("\"%s\" doesn't match expected \"%s\"",
					snotes, expected)
			}
			i1.gotNotes = true
		case n2 := <-i2.notesChan:
			t.Log("i2 complete")
			if !waitTimer.Stop() {
				<-waitTimer.C
			}
			waitTimer.Reset(time.Second * 10)
			snotes := string(bytes.TrimRight(n2.Bytes(), "\n"))
			expected := "i2: commitI: " + strconv.Itoa(i2.commitI)
			if snotes != expected {
				t.Fatalf("\"%s\" doesn't match expected \"%s\"",
					snotes, expected)
			}
			i2.gotNotes = true
		case <-waitTimer.C:
			t.Fatal("timeout while waiting for icyCI notes\n")
		}

		if i1.gotNotes && i2.gotNotes {
			for _, i := range []*instanceState{&i1, &i2} {
				checkResults(t, cloneDir, i.commit,
					i.params.testScript)
				// tell icyCI eventLoop to end
				i.evExitChan <- 1
				i.wg.Wait()
			}
			return
		}
	}
}

// - check that ICYCI_X env variables are set within script
func TestScriptEnv(t *testing.T) {
	// commitI tracks the number of commits for which we should expect a
	// corresponding results note entry.
	var curCommit string

	tdir, err := ioutil.TempDir("", "icyci-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tdir)

	gpgInit(t, tdir)

	sdir := path.Join(tdir, "test_src_and_rslt")
	rdir := sdir
	gitReposInit(t, tdir, sdir)

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
	curCommit = fileWriteSignedCommit(t, sdir, "src_test.sh",
		`echo "ICYCI_PID: $ICYCI_PID"`)

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
			snotes := string(bytes.TrimRight(notes.Bytes(), "\n"))
			if snotes != "ICYCI_PID: "+strconv.Itoa(os.Getpid()) {
				t.Fatalf("%s does not match expected\n", snotes)
			}

			// Finished, tell icyCI eventLoop to end
			evExitChan <- 1
			wg.Wait()
			return

		case <-notesWaitTimer.C:
			t.Fatal("timeout while waiting for icyCI notes\n")
		}
	}
}
