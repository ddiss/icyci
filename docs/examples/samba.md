Samba Testing with icyCI
------------------------

Import the GPG key used for Samba release tag signing:
```sh
wget https://download.samba.org/pub/samba/samba-pubkey.asc
gpg2 --import samba-pubkey.asc
```

In case you haven't already, ensure that the user running icyCI has a Git
user.name configured:
```sh
git config --global user.name "icyCI"
```

Create a new Git repository to store the test results. This example uses a local
directory, but a remote repository could also be used.
```sh
git init --bare ~/icyci-samba-results
```

Write a test script. The example below compiles Samba and runs *make test*:
```sh
echo '#!/bin/bash
	./configure.developer
	make -j4
	make test' > ~/test-samba.sh
chmod 755 ~/test-samba.sh
```

Start icyCI, pointing it at a release branch of the upstream samba repository,
the test script, and the results repository:
```sh
icyci -source-repo git://git.samba.org/samba \
	-source-branch v4-11-stable \
	-test-script ~/test-samba.sh \
	-results-repo ~/icyci-samba-results
```

[Wait for icyCI to finish running ~/test-samba.sh and push results]

Add the *results-repo* as a new remote to an existing clone of the
*source-repo*:
```sh
# The samba directory is an existing clone of git://git.samba.org/samba
cd samba
git remote add icyci-results ~/icyci-samba-results
git fetch icyci-results "refs/notes/*:refs/notes/*"
```

The icyCI test results can be viewed alongside the regular git log output with:
```sh
git log --show-notes="*"
```


Testing master
--------------

Currently the master branch of the Samba repository doesn't carry GPG
signatures, so as an alternative you could have icyCI track a clone / mirror
of master, which you periodically tag yourself.
