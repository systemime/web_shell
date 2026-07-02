import assert from 'node:assert/strict';
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';

import { HttpError, makePathGuard } from '../src/security.js';

test('path guard rejects traversal and symlink escape', async () => {
  const root = await fsp.mkdtemp(path.join(os.tmpdir(), 'web-worker-'));
  const outside = await fsp.mkdtemp(path.join(os.tmpdir(), 'web-worker-out-'));
  await fsp.writeFile(path.join(root, 'ok.txt'), 'ok');
  await fsp.symlink(outside, path.join(root, 'outside'));

  const guard = makePathGuard(root);
  assert.equal((await guard.existing('ok.txt')).rel, 'ok.txt');
  await assert.rejects(() => guard.existing('../etc/passwd'), HttpError);
  await assert.rejects(() => guard.existing('outside'), HttpError);

  await fsp.rm(root, { recursive: true, force: true });
  await fsp.rm(outside, { recursive: true, force: true });
});
