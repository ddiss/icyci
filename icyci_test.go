// SPDX-License-Identifier: AGPL-3.0-only
//
// Copyright (C) 2019 SUSE LLC

package main

import (
	"bytes"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"sync"
	"time"
	"testing"
)

const (
	userName  = "icyCI test"
	userEmail = "icyci@example.com"
)

func gpgInit(t *testing.T, gpgDir string) {
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
			name = ` + userName + `
		        email = ` + userEmail + `
			signingKey = <` + userEmail + `>`),
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

func waitNotes(t *testing.T, repoDir string, notesRef string,
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

		t.Logf("checking notes")
		cmd = exec.Command("git", "notes", "--ref=" + notesRef, "show")
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

	// GNUPGHOME for key import and verification
	gpgDir := path.Join(tdir, "gpg")

	// export HOME and GNUPGHOME to ensure that our custom git + gpg configs
	// are picked up for all git operations.
	os.Setenv("HOME", tdir)
	os.Setenv("GNUPGHOME", gpgDir)
	os.Setenv("GIT_PAGER", "")

	gpgInit(t, gpgDir)

	sdir := path.Join(tdir, "test_src")
	rdir := path.Join(tdir, "test_rslt")
	gitReposInit(t, tdir, sdir, rdir)

	srcTest := path.Join(sdir, "src_test.sh")
	err = ioutil.WriteFile(srcTest,
		[]byte(`#!/bin/bash
			echo "this has been run by icyci"`),
		os.FileMode(0755))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "checkout", "-b", "mybranch")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("git", "add", "src_test.sh")
	cmd.Dir = sdir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("git", "commit", "-a", "-S", "-m",
		"signed source commit")
	cmd.Dir = sdir
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
		waitNotes(t, cloneDir, stdoutNotesRef, notesChan)
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
