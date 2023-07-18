Multi-Instance Usage
====================

icyCI can run with a *source-repo* / *source-branch* / *results-repo*
combination as a single instance, or as one of multiple instances possibly
spread across multiple hosts.
This document outlines multi-instance usage, which can generally be broken up
into two job types: [exclusive](#exclusive-jobs) and
[simultaneous](#simultaneous-jobs).


Exclusive Jobs
--------------

* `-test-script` will be run only once per verified commit, by whichever
  instance first detects the new commit and successfully pushes a *.lock* note
  to the *results-repo*.
* This configuration makes sense when multiple invocations of `-test-script`
  for a commit would be wasteful, and all instances and jobs are uniform.
* This configuration is the default when icyCI is run multiple times with the
  same `-notes-ns` namespace parameter, which defaults to `icyci`.


Simultaneous Jobs
-----------------

* `-test-script` can run in parallel across multiple instances with unique
  `-notes-ns` namespaces.
* This makes sense if instances carry different characteristics, e.g. differing
  architectures, or `-test-script` behaviour.
* Exclusive and simultaneous jobs can be mixed and matched across instances.

The following example uses separate `icyci-aarch64` and `icyci-x86-64` notes
namespaces to allow `test-linux.sh` to run independently on separate hosts
against verified commits in the Linux kernel repository:

```sh
aarch64-host> icyci -notes-ns icyci-aarch64 \
  -source-repo git://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git \
  -source-branch master \
  -test-script ~/test-linux.sh \
  -results-repo results-host.example.com:/icyci-linux-results
```

```sh
amd64-host> icyci -notes-ns icyci-x86-64 \
  -source-repo git://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git \
  -source-branch master \
  -test-script ~/test-linux.sh \
  -results-repo results-host.example.com:/icyci-linux-results
```

Each instance pushes `test-linux.sh` results to the same *results-repo*, which
can be filtered when viewing via e.g.
```sh
# show results for both instances:
dev-host> git log --show-notes="icyci*"
# show results for aarch64 only:
dev-host> git log --show-notes="icyci-aarch64.*"
```


Further Recommendations
-----------------------

If multiple icyCI instances are deployed on a single host with the same remote
`-source-repo`, then consider setting up a local mirror and using the
`-source-reference` parameter to reduce network and local storage resource
consumption; instances will obtain *refs* from the local mirror, if available.
