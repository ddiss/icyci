icyCI can be configured to run as a systemd service.
The procedure is as follows:

1. Install the `system/icyci@.service` file into the systemd units path, e.g.
   `/etc/systemd/system/icyci@.service`.
   * The service is configured to run as local user `icyci`. Use
     `systemctl edit --drop-in=user icyci@.service` if a different user is
     desired.

2. Copy `icyci-instance.conf` under `/etc/icyci/`, using a name that identifies
   the Git repository that it tests, e.g.
   `/etc/icyci/linux-kernel-stable.conf`
   * Edit the instance configuration file and set the parameters based on what
     and how you wish to test. E.g.
   ```
   SOURCE_REPO="git://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git"
   SOURCE_BRANCH="linux-5.3.y"
   TEST_SCRIPT="~/build-linux.sh"
   RESULTS_REPO="~/icyci-linux-results"
   ```
   * Multiple instance config files can be created for each Git repository that
     you wish to test.

3. Enable and start the systemd unit, specifying the instance name used in step
   (2). E.g.
   ```
   # systemctl enable icyci@linux-kernel-stable
   # systemctl start icyci@linux-kernel-stable
   ```

## Running as a user service

  ```
  # mkdir -p $HOME/.config/systemd/user $HOME/.config/icyci
  # ln -sr systemd/user/icyci@.service $HOME/.config/systemd/user/icyci@linux-kernel-stable.service
  # cp systemd/icyci-instance.conf $HOME/.config/icyci/linux-kernel-stable.conf
  # systemctl --user enable icyci@linux-kernel-stable.service
  # loginctl enable-linger $USER
  ```
  * the last command applies only to remote machines without permanent login
