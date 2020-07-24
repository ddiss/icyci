icyCI
=====

icyCI has one purpose:
- Pull and verify source from a Git repository
- Run a build/test/whatever script
- Push script results to a Git repository
- Poll for new source changes

It differs from other CI utilities in a few ways:

| What?          | How?                                                        |
| -------------- | ----------------------------------------------------------- |
| Lightweight    | Single Go binary with a Git core tools dependency.          |
| Secure         | Only proceeds if branch GPG signature can be verified.      |
| Trustless      | Doesn't need push access to source repositories. Rootless.  |
| Vendor Neutral | Only relies on standard Git features and protocols.         |
| Fast           | Asynchronous, event based architecture.                     |
| Informative    | Results published as git-notes, viewable from git log.      |
| Simple         | Set and forget. Polls and tests new changes automatically.  |
| Distributed    | Can cooperatively run on a single or multiple hosts.        |


Usage
-----

The example below demonstrates how icyCI can be used to test the Linux kernel.

Import GPG keys used for source repository signing. Only Linus' and Greg's keys
are imported below:
```sh
gpg2 --locate-keys torvalds@kernel.org gregkh@kernel.org
```

In case you haven't already, ensure that the user running icyCI has a Git
user.name configured:
```sh
git config --global user.name "icyCI"
```

Create a new Git repository to store the test results. This example uses a local
directory, but a remote repository could also be used.
```sh
git init --bare ~/icyci-linux-results
```

Write a test script. The example below only performs a kernel build:
```sh
echo '#!/bin/bash
      make defconfig && make -j4' > ~/build-linux.sh
chmod 755 ~/build-linux.sh
```

Start icyCI, pointing it at a branch of the stable kernel repository, the test
script, and the results repository:
```sh
icyci -source-repo git://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git \
	-source-branch linux-5.3.y \
	-test-script ~/build-linux.sh \
	-results-repo ~/icyci-linux-results
```

[Wait for icyCI to push results, after which it'll poll for source changes]

To view the test script results, you can either do a fresh clone of the
*results-repo*:
```sh
git clone ~/icyci-linux-results ~/linux-results
cd ~/linux-results
git fetch origin "refs/notes/*:refs/notes/*"
```

...or alternatively add the *results-repo* as a new remote to an existing clone
of the *source-repo*:
```sh
# The linux directory is an existing clone of
# git://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git
cd linux
git remote add icyci-results ~/icyci-linux-results
# git doesn't fetch notes by default, so configure it to do so...
git config --add remote.icyci-results.fetch "refs/notes/*:refs/notes/*"
git fetch icyci-results
```

The icyCI test results can be viewed alongside the regular git log output with:
```sh
git log --show-notes="*"
```

icyCI can be run as a systemd service. For details, see
[docs/systemd-usage.md](docs/systemd-usage.md).


Architecture
------------

icyCI runs as a relatively simple state machine:
```
               clone source branch
                           ↓
     ↗---→---→ check GPG signature on branch tip  → can't verify ---↘
     |                     ↓                                        |
     |         push lock to results repository    → already locked →|
     |                     ↓                                        |
     ↑         run test script                                      ↓
     |                     ↓                                        |
     |         add captured script output via git-notes             |
     |                     ↓                                        |
     ↑         push notes and source results repository             |
    new                    ↓                                        |
  commits      cleanup source repository                            |
     |                     ↓                                        |
     ↖---←---← fetch commits in source branch ←---←---←---←----←----↙
                           ↺
                        no new
                        commits
```


Future
------

- Improve self test functionality
- Improve documentation
- Support multiple branches within one instance
  - Determine applicable via:
```
git for-each-ref --no-contains=X --count=1 --sort=-committerdate refs/heads/
```
- push results as static website, in addition to git-notes
- Save and push build/test artifacts, in addition to output
- Periodically push progress while testing
- Improve logging
- Your feature; please raise requests via the issue tracker


License
-------

icyCI is Free Software, published under the GNU Affero General Public License
v3.0.
This license applies to icyCI source itself; it does not affect programs tested
by icyCI.
