# Running the runner with launchd on macOS

[`com.gitea.runner.plist`](./com.gitea.runner.plist) runs the runner in the
login session of the user that jobs should run as. Jobs then have that user's
login keychain and the GUI.

## Setup

1. Install the `gitea-runner` binary (e.g. to `/usr/local/bin/gitea-runner`).
2. Register as that user, with a host label so that jobs run on the Mac itself:

   ```bash
   mkdir ~/gitea-runner && cd ~/gitea-runner
   gitea-runner register --instance https://gitea.example.com --token <token> --labels macos-latest:host
   ```

3. Replace `/Users/gitea-runner` in the file with the user's home directory,
   then install it and start the runner. From now on, it also starts whenever
   the user logs in:

   ```bash
   mkdir -p ~/Library/LaunchAgents
   cp com.gitea.runner.plist ~/Library/LaunchAgents/
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.gitea.runner.plist
   ```

4. Enable automatic login for the user in System Settings, so that the runner
   starts after a restart too. macOS does not offer automatic login while
   FileVault is on.

The runner does not get the environment of your shell. Set what jobs need, such
as proxy variables, in `EnvironmentVariables`.

## Local network access

macOS blocks a runner that was started this way from other hosts on the Mac's
own subnet, such as the Gitea instance or a shared cache server. The runner
then logs `no route to host`. Commands run in Terminal or over SSH are not
blocked, so registering works even when the running runner cannot connect. See
[local network privacy](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).

macOS asks on the Mac's screen whether to allow the access. To allow it without
that prompt, exempt the subnet on macOS 15.5 or later and restart the Mac. On
Wi-Fi, the key is `AllowedWiFiLocalNetworkAddresses`:

```bash
sudo defaults write com.apple.network.local-network AllowedEthernetLocalNetworkAddresses -array "192.168.1.0/24"
```

## Git credentials

Git on macOS saves credentials from remote URLs to the keychain, which can stop
a job at a dialog that nobody answers. To turn that off, set
`GIT_CONFIG_NOSYSTEM` to `1` in `EnvironmentVariables`.
