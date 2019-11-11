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

Create a new repository to store your test results. This example uses a local
directory, but a remote host could also be used.
```
> mkdir ~/icyci-results
> git init --bare ~/icyci-results
```

Sign a commit at the HEAD of the branch that you wish to test:
```
> git commit -S -m "my signed commit"
> git push <source repo>
```

Start icyCI, pointing it at your source repo, a build/test script as well as a
results repository:
```
> icyci -source-repo <source repo> -source-branch <my branch> -results-repo ~/icyci-results -test-script <my build/test program>
```

[wait for icyCI to complete]

Add the results repository as a new remote to your checked out source (where
you signed the commit).
```
> git remote add icyci-results ~/icyci-results
```

Fetch the icyCI results:
```
> git fetch icyci-results  "refs/notes/*:refs/notes/*"
```

The icyCI test results can now be viewed from your source repository alongside
the regular git log output, with:
```
git log --show-notes="*"
```


Future
------

- poll for source repository updates (coming soon)
- improve documentation
- Support multiple branches within one instance
- Save and push build/test artifacts, in addition to output
- periodically push progress while testing
- Your feature; please raise requests via the issue tracker


License
-------

icyCI is Free Software, published under the AGPLv3 license.
