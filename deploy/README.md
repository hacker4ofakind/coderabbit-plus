# Self-hosted GitHub reviews

Run one service per Codex login on a 64-bit Linux host with Go, Git, Docker Engine with BuildKit, and Python 3. The service listens on loopback. Tailscale Funnel can publish the signed webhook endpoint. The reviewer uses your ChatGPT allowance and supports trusted private repos with same-repo branches.

## Build the runner

Run the build from your normal Pi login in the source checkout. If you copied the skill with SCP into your home directory, use the command below. The build copies both skills into the image and checks the installed tools.

```sh
docker build --tag coderabbit-plus:runner \
  --build-context vibe_security="$HOME/vibe-security" .
go build -o coderabbit-plus .
```

## Create the service account

Run these commands once from your normal Pi login. They create a system account with a private primary group, no home directory, and no login shell. Give this account access to the Pi's system Docker daemon through the `docker` group. Docker socket access grants root-level control of the host. Keep that access with the trusted host service. Review containers have their own user and receive no Docker socket. [Docker Engine permissions](https://docs.docker.com/engine/install/linux-postinstall/#manage-docker-as-a-non-root-user)

```sh
sudo useradd --system --user-group --no-create-home \
  --home-dir /nonexistent --shell /usr/sbin/nologin coderabbit-plus
sudo usermod -aG docker coderabbit-plus
sudo install -o root -g root -m 0755 coderabbit-plus /usr/local/bin/coderabbit-plus
sudo install -d -m 0755 /var/lib/coderabbit-plus
sudo install -d -m 0700 -o coderabbit-plus -g coderabbit-plus /var/lib/coderabbit-plus/spool
sudo install -d -m 0750 -o root -g coderabbit-plus /etc/coderabbit-plus
sudo install -d -m 0700 -o 10001 -g 10001 /var/lib/coderabbit-plus/codex
sudo install -o root -g coderabbit-plus -m 0640 \
  service-config.example.json /etc/coderabbit-plus/config.json
```

The host account owns the queue. UID and GID `10001` belong to the reviewer inside the image and own the mounted Codex login directory.

Check that the service account can see the image you built. A successful result means you can use that build as-is. Docker images are shared by users of the same daemon.

```sh
sudo -u coderabbit-plus -- docker --host unix:///var/run/docker.sock \
  image inspect coderabbit-plus:runner --format '{{.Id}}'
```

If your usual Docker context uses a separate daemon, such as a rootless daemon, transfer the image to the system daemon and repeat the check.

```sh
docker image save coderabbit-plus:runner | \
  sudo docker --host unix:///var/run/docker.sock image load
```

## Check Docker and sign in

Run the offline scratch probe before using your subscription. It mounts a temporary Codex home and exercises the actual Codex Linux sandbox.

```sh
sudo -u coderabbit-plus -- env DOCKER_HOST=unix:///var/run/docker.sock \
  /usr/local/bin/coderabbit-plus check-runner /etc/coderabbit-plus/config.json
```

Docker's default seccomp profile can block Bubblewrap from creating its nested namespaces. If the check fails with a namespace error, use the [Docker Engine default profile](https://github.com/moby/moby/blob/master/vendor/github.com/moby/profiles/seccomp/default.json) for your installed Engine release as the input below. The helper retains the baseline filters and adds the namespace and mount syscalls Bubblewrap needs. Set `seccomp_profile` in the config to the resulting absolute path and rerun the probe.

```sh
python3 deploy/runner-seccomp.py /path/to/docker-default.json |
  sudo tee /etc/coderabbit-plus/runner-seccomp.json >/dev/null
sudo chgrp coderabbit-plus /etc/coderabbit-plus/runner-seccomp.json
sudo chmod 0640 /etc/coderabbit-plus/runner-seccomp.json
sudoedit /etc/coderabbit-plus/config.json
```

The host must also permit unprivileged user namespaces. If the host enforces AppArmor and its policy blocks the probe, `apparmor_profile` can name a loaded, confined policy that permits Bubblewrap's nested mounts. The runner uses a non-root UID, a read-only image filesystem, and zero Docker capabilities.

Enable device code login in your ChatGPT security settings. Then sign in once on this runner and complete the code flow in your browser. Keep this dedicated directory between jobs so Codex can update its session.

```sh
sudo -u coderabbit-plus -- docker --host unix:///var/run/docker.sock \
  run --rm --pull=never -it \
  --mount type=bind,source=/var/lib/coderabbit-plus/codex,target=/codex-home \
  --entrypoint codex coderabbit-plus:runner login --device-auth \
  -c 'cli_auth_credentials_store="file"'
```

The mounted Codex home contains your active account credentials. Use it only for this trusted private reviewer. The host keeps the GitHub App key and webhook secret in a separate directory. The review container receives a read-only copy of the fetched Git objects, its job-local checkout and scratch space, and the Codex home.

## Register the GitHub App and expose the webhook

1. Register an app with repository permissions **Contents** read-only and **Pull requests** read and write. Subscribe to **Pull request** events. Select **Any account** if you want the same app on multiple organizations.
2. Generate and download a GitHub App private key. From the Pi, install the downloaded file and generate a webhook secret once. Replace the PEM source path with the location of your downloaded key. Enter the secret shown by the last command in GitHub's webhook settings.

   ```sh
   sudo install -o root -g coderabbit-plus -m 0640 \
     /path/to/downloaded-private-key.pem /etc/coderabbit-plus/github-app.pem
   sudo sh -ec 'umask 027; set -C; openssl rand -hex 32 > /etc/coderabbit-plus/webhook-secret'
   sudo chgrp coderabbit-plus /etc/coderabbit-plus/webhook-secret
   sudo cat /etc/coderabbit-plus/webhook-secret
   ```

3. Install the app on selected private repositories. Run `sudoedit /etc/coderabbit-plus/config.json` to set the app ID and slug and add one allowlist entry per repo. The installation ID is the trailing number in the installation's settings URL. Repos covered by the same installation share that ID. To look up each numeric `repository_id`, run this on a machine where `gh` is signed in with access to that private repo. Replace `OWNER/REPO` with its name on GitHub.

   ```sh
   gh api repos/OWNER/REPO --jq .id
   ```

   Enter the result as a JSON number and set `repository` to `OWNER/REPO`. Keep `automatic: false` for the initial test.
4. Install and start the systemd service from your checkout. It runs as `coderabbit-plus` and starts again when the Pi reboots.

   ```sh
   sudo install -o root -g root -m 0644 deploy/coderabbit-plus.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now coderabbit-plus.service
   sudo systemctl status coderabbit-plus.service --no-pager
   ```

   Read its logs with `sudo journalctl -u coderabbit-plus.service -f`. After changing the configuration, run `sudo systemctl restart coderabbit-plus.service`.

5. Add the `/coderabbit-plus` route on Funnel's standard HTTPS port. If port 443 is already a public HTTPS Funnel, this adds a path on the same listener. Check `tailscale funnel status` first if you already use that path. Specify the full local target URL because Funnel strips the public path before forwarding. If port 443 serves a private Tailscale Serve site, keep that site private and choose a different Funnel port.

   ```sh
   sudo tailscale funnel --bg --https=443 --set-path=/coderabbit-plus \
     http://127.0.0.1:8876/github/webhook
   tailscale funnel status
   ```

6. Set the GitHub App webhook URL to `https://YOUR-PI.YOUR-TAILNET.ts.net/coderabbit-plus` with no trailing slash. Check recent deliveries for a successful ping after the service is up. If you already registered the app with a different URL, edit it in the app settings and redeliver the ping.

## Trigger reviews

- Create the `coderabbit-plus:full` and `coderabbit-plus:commit` labels, then apply either to a ready PR. The first reviews the full PR diff. The second reviews the head commit against its first parent.
- Turn on `automatic` per repository and restart the service to review ready PRs and new pushes. New pushes wait for the debounce interval and review from the last covered head. If that head is unavailable or has been rewritten, the worker reviews the full PR. Commit-only reviews leave the incremental baseline in place.
- CodeRabbit Plus posts a status comment as soon as it accepts a review, including during the debounce window. It updates the same comment as the review starts, posts, or fails. Status updates use the app's existing **Pull requests** write permission.
- Each GitHub review includes mode and both commit SHAs as a regular review comment. The queue survives a process restart, retries transient failures twice, and checks whether the bot already posted before repeating a job. It discards obsolete snapshots before reviewing and checks the PR again before posting. If the Pi was offline, use GitHub's webhook delivery history to redeliver the event. After a failed manual run, remove and reapply the label to retry.
