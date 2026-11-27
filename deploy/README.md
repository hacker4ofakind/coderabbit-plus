# Self-hosted GitHub reviews

Run one service per Codex login on a 64-bit Linux host with Go, Git, Docker Engine with BuildKit, and Python 3. The service listens on loopback. Tailscale Funnel can publish the signed webhook endpoint. The reviewer uses your ChatGPT allowance and supports trusted private repos with same-repo branches.

## Build the runner

On the Pi, point the named build context at your trusted copy of the entire vibe-security skill directory. Include its `SKILL.md` and `references/` when copying it from your machine. The build copies both skills into the image and checks the installed tools. The build context needs no authentication files.

```sh
docker build --tag coderabbit-plus:runner \
  --build-context vibe_security=/path/to/vibe-security .
go build -o coderabbit-plus .
```

For a production setup, run the service as a dedicated host user with access to Docker. The example configuration uses the paths below. Run these commands as that service user with sudo for directory creation, or adjust ownership to match your service account.

```sh
sudo install -d -m 0755 /var/lib/coderabbit-plus
sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" \
  /etc/coderabbit-plus /var/lib/coderabbit-plus/spool
sudo install -d -m 0700 -o 10001 -g 10001 /var/lib/coderabbit-plus/codex
cp service-config.example.json /etc/coderabbit-plus/config.json
```

## Check Docker and sign in

Run the offline scratch probe before using your subscription. It mounts a temporary Codex home and exercises the actual Codex Linux sandbox.

```sh
./coderabbit-plus check-runner /etc/coderabbit-plus/config.json
```

Docker's default seccomp profile can block Bubblewrap from creating its nested namespaces. If the check fails with a namespace error, use the [Docker Engine default profile](https://github.com/moby/moby/blob/master/vendor/github.com/moby/profiles/seccomp/default.json) for your installed Engine release as the input below. The helper retains the baseline filters and adds the namespace and mount syscalls Bubblewrap needs. Set `seccomp_profile` in the config to the resulting absolute path and rerun the probe.

```sh
python3 deploy/runner-seccomp.py /path/to/docker-default.json \
  > /etc/coderabbit-plus/runner-seccomp.json
```

The host must also permit unprivileged user namespaces. If the host enforces AppArmor and its policy blocks the probe, `apparmor_profile` can name a loaded, confined policy that permits Bubblewrap's nested mounts. The runner uses a non-root UID, a read-only image filesystem, and zero Docker capabilities.

Enable device code login in your ChatGPT security settings. Then sign in once on this runner and complete the code flow in your browser. Keep this dedicated directory between jobs so Codex can update its session.

```sh
docker run --rm --pull=never -it \
  --mount type=bind,source=/var/lib/coderabbit-plus/codex,target=/codex-home \
  --entrypoint codex coderabbit-plus:runner login --device-auth \
  -c 'cli_auth_credentials_store="file"'
```

The mounted Codex home contains your active account credentials. Use it only for this trusted private reviewer. The host keeps the GitHub App key and webhook secret in a separate directory. The review container receives a read-only copy of the fetched Git objects, its job-local checkout and scratch space, and the Codex home.

## Register the GitHub App and expose the webhook

1. Register an app with repository permissions **Contents** read-only and **Pull requests** read and write. Subscribe to **Pull request** events. Select **Any account** if you want the same app on multiple organizations.
2. Generate a GitHub App private key and save it at `private_key_file`. Generate a random webhook secret, save it at `webhook_secret_file`, and enter the same value in the app settings. Keep both files readable only by the service user.
3. Install the app on selected private repositories. In the service config, set the app ID and slug and add an allowlist entry for each repo. The installation ID is the number in the installation's settings URL. The repository ID is the numeric `id` from GitHub's Get a repository REST endpoint. Use `automatic: false` for the initial test.
4. Run the service on the Pi. Once verified, supervise this same command with your usual service manager.

   ```sh
   ./coderabbit-plus serve /etc/coderabbit-plus/config.json
   ```

5. Point Funnel to the loopback listener. For a dedicated Funnel HTTPS port, the command below preserves the requested path when forwarding to the service. If you already use port 443 for something else, use a free Funnel port such as 8443 and include it in the GitHub webhook URL.

   ```sh
   tailscale funnel --bg 8876
   ```

6. Set the GitHub App webhook URL to the Funnel URL followed by `/github/webhook`. Check the app's recent deliveries for a successful ping. The public listener accepts only this signed POST route.

## Trigger reviews

- Create the `coderabbit-plus:full` and `coderabbit-plus:commit` labels, then apply either to a ready PR. The first reviews the full PR diff. The second reviews the head commit against its first parent.
- Turn on `automatic` per repository and restart the service to review ready PRs and new pushes. New pushes wait for the debounce interval and review from the last covered head. If that head is unavailable or has been rewritten, the worker reviews the full PR. Commit-only reviews leave the incremental baseline in place.
- Each GitHub review includes mode and both commit SHAs as a regular review comment. The queue survives a process restart, retries transient failures twice, and checks whether the bot already posted before repeating a job. It discards obsolete snapshots before reviewing and checks the PR again before posting. If the Pi was offline, use GitHub's webhook delivery history to redeliver the event. After a failed manual run, remove and reapply the label to retry.
