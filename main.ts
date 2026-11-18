// main.ts — the entrypoint the harness runs (`node /bot/main.ts`).
//
// It launches the Go forge binary (`forge`, a statically linked Linux amd64
// binary committed beside this file) and waits for it to exit, propagating its
// exit code. The binary does the real work: it binds an ephemeral port,
// registers it with the env, serves the forge, polls for done, and exits 0.
//
// The binary may arrive without the executable bit (a browser or API upload
// creates files mode 100644), and /bot is read-only in the trial pod, so the
// launcher cannot chmod it in place. It therefore copies the binary to a
// writable temp path, chmods the copy, and execs that. If the committed binary
// is already executable it is used directly.

import { chmodSync, copyFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const here = fileURLToPath(new URL('.', import.meta.url));
const binaryPath = join(here, 'forge');

function launchable(path: string): string {
  // Already executable: use it as-is.
  try {
    chmodSync(path, 0o755);
    return path;
  } catch {
    // /bot is read-only (or the file is not ours to chmod): copy to tmp.
  }
  const dest = join(tmpdir(), `forge-${process.pid}`);
  copyFileSync(path, dest);
  chmodSync(dest, 0o755);
  return dest;
}

if (!existsSync(binaryPath)) {
  process.stderr.write('[entry] forge binary is missing from the repository\n');
  process.exit(1);
}

const executable = launchable(binaryPath);
const child: ChildProcess = spawn(executable, [], { stdio: 'inherit' });
child.on('error', (err) => {
  process.stderr.write(`[entry] failed to launch the forge binary: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal) process.exit(1);
  process.exit(code ?? 1);
});
