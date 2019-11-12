icyCI
=====

icyCI has one job:
- Pull code from a Git repository
- Run a build/test/whatever script
- Push the results to a Git repository

It differs from other CI utilities in a few ways:

| What?          | How?                                                        |
| -------------- | ----------------------------------------------------------- |
| Lightweight    | Single binary with Git and Go standard library dependencies |
| Secure         | Only proceeds if branch GPG signature can be verified       |
| Trustless      | Doesn't need push access to source repositories. Rootless.  |
| Vendor Neutral | Only relies on standard Git features and protocols          |
| Fast           | Asynchronous, event based architecture                      |
| Informative    | Results published as git-notes, viewable from git log       |


Usage
-----

The example below demonstrates how icyCI can be used to test the Linux kernel.

Import GPG keys used for source repository signing. Only Linus' and Greg's keys
are imported below:
```sh
gpg2 --locate-keys torvalds@kernel.org gregkh@kernel.org
```

In case you haven't already, ensure that the user running icyCI has
a git user.name:
```sh
git config --global user.name "icyCI"
```

Create a new repository to store the test results. This example uses a local
directory, but a remote host could also be used.
```sh
git init --bare ~/icyci-linux-results
```

Write a test script. The example below only performs a kernel build:
```sh
echo '#!/bin/bash
      make defconfig && make -j4' > ~/build-linux.sh
chmod 755 ~/build-linux.sh
```

Start icyCI, pointing it at Linus' mainline kernel repository, the test script
as well as the results repository:
```sh
icyci -source-repo git://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git -source-branch master -results-repo ~/icyci-linux-results -test-script ~/build-linux.sh
```

[Wait for icyCI to complete]

Go to a checkout of the source repository. This can be on any machine with
access to the source and results repositories:
```sh
git clone git://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git
cd linux
```

Add the results repository as a new remote to your source repository:
```sh
git remote add icyci-results ~/icyci-linux-results
```

Fetch the icyCI results:
```sh
git fetch icyci-results "refs/notes/*:refs/notes/*"
```

The icyCI test results can now be viewed from your source repository alongside
the regular git log output, with:
```sh
git log --show-notes="*"
```


Future
------

- Poll for source repository updates (coming soon)
- Improve documentation
- Support multiple branches within one instance
- Save and push build/test artifacts, in addition to output
- Periodically push progress while testing
- Your feature; please raise requests via the issue tracker


License
-------

icyCI is Free Software, published under the GNU Affero General Public License
v3.0.
This license applies to icyCI source itself; it does not affect programs tested
by icyCI.
