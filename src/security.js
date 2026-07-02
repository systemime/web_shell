import fs from 'node:fs';
import fsp from 'node:fs/promises';
import path from 'node:path';

export class HttpError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

export function isInside(root, target) {
  const rel = path.relative(root, target);
  return rel === '' || (!!rel && !rel.startsWith('..') && !path.isAbsolute(rel));
}

export function makePathGuard(rootDir) {
  const root = fs.realpathSync(path.resolve(rootDir));

  function lexical(userPath = '.') {
    const clean = String(userPath || '.').replace(/^[/\\]+/, '');
    const full = path.resolve(root, clean);
    if (!isInside(root, full)) throw new HttpError(403, 'Path escapes workspace root');
    return full;
  }

  function rel(full) {
    return path.relative(root, full).split(path.sep).filter(Boolean).join('/');
  }

  async function existing(userPath = '.') {
    const full = lexical(userPath);
    let real;
    try {
      real = await fsp.realpath(full);
    } catch (err) {
      if (err.code === 'ENOENT') throw new HttpError(404, 'Path not found');
      throw err;
    }
    if (!isInside(root, real)) throw new HttpError(403, 'Path escapes workspace root');
    return { full, real, rel: rel(full) };
  }

  async function writable(userPath) {
    const full = lexical(userPath);
    const parent = path.dirname(full);
    let parentReal;
    try {
      parentReal = await fsp.realpath(parent);
    } catch (err) {
      if (err.code === 'ENOENT') throw new HttpError(404, 'Parent directory not found');
      throw err;
    }
    if (!isInside(root, parentReal)) throw new HttpError(403, 'Path escapes workspace root');
    return { full, rel: rel(full) };
  }

  return { root, existing, writable, rel };
}
