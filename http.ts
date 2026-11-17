// http.ts — the request context every handler shares, plus the response helpers.
//
// No wall clock, no randomness: request ids are a per-process counter, so two
// runs over the same store produce identical bytes.

import type { IncomingHttpHeaders, ServerResponse } from 'node:http';
import type { Store } from './store.ts';

export interface Ctx {
  readonly method: string;
  readonly path: string;
  /** Decoded path segments (no leading empty segment, no trailing empty segment). */
  readonly segments: string[];
  readonly query: URLSearchParams;
  readonly headers: IncomingHttpHeaders;
  /** Raw request body (empty when the client sent none). */
  readonly body: Buffer;
  /** The Host header, for building absolute URLs in Link headers. */
  readonly host: string;
  /** The request id stamped on every response (req-<counter>). */
  readonly requestId: string;
}

export function newRequestId(store: Store): string {
  return `req-${store.nextRequestId++}`;
}

export function sendJson(res: ServerResponse, status: number, body: unknown, requestId: string, extra?: Record<string, string>): void {
  const text = JSON.stringify(body);
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'Content-Length': String(Buffer.byteLength(text)),
    'X-GitHub-Request-Id': requestId,
    ...(extra ?? {}),
  };
  res.writeHead(status, headers);
  res.end(text);
}

export function sendBuffer(res: ServerResponse, status: number, contentType: string, body: Buffer, requestId: string): void {
  res.writeHead(status, {
    'Content-Type': contentType,
    'Content-Length': String(body.length),
    'X-GitHub-Request-Id': requestId,
  });
  res.end(body);
}

export function sendEmpty(res: ServerResponse, status: number, requestId: string): void {
  res.writeHead(status, { 'X-GitHub-Request-Id': requestId });
  res.end();
}

/** Parse a JSON body. Empty bodies are `{}` for convenience; malformed JSON is an error. */
export function parseJsonBody(body: Buffer): { ok: true; value: unknown } | { ok: false; error: string } {
  const text = body.toString('utf8');
  if (text.trim() === '') return { ok: true, value: {} };
  try {
    return { ok: true, value: JSON.parse(text) };
  } catch {
    return { ok: false, error: 'Malformed JSON in request body' };
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

export function asBoolean(value: unknown): boolean | undefined {
  return typeof value === 'boolean' ? value : undefined;
}

export function asNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

export function asStringArray(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) return undefined;
  if (value.some((item) => typeof item !== 'string')) return undefined;
  return value as string[];
}
