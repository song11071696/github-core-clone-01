// graphql.ts — a small hand-written GraphQL executor for the conformance checks.
//
// It parses the query documents the kit sends (queries and mutations, variables,
// aliases, inline fragments, lists and input objects), executes them against the
// same in-memory store the REST and git endpoints use, and answers with
// `{data, errors}`. Unknown fields fail with the GraphQL-style message
// `Cannot query field "x" on type "Y".` Global ids are opaque base64 strings
// with a type prefix; they are stable across reads.

import type { ServerResponse } from 'node:http';
import {
  FIXED_TIMESTAMP,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  diffTrees,
  pullMergeability,
  squashMergePull,
  type Issue,
  type IssueComment,
  type PullRequest,
  type Repo,
  type Store,
} from './store.ts';
import { parseCommit } from './gitpack.ts';
import { isRecord, parseJsonBody, sendJson, type Ctx } from './http.ts';

// ---------------------------------------------------------------------------------------------
// lexer + parser
// ---------------------------------------------------------------------------------------------

type Token =
  | { kind: 'punct'; value: string }
  | { kind: 'name'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'number'; value: number };

function tokenize(input: string): Token[] {
  const tokens: Token[] = [];
  let index = 0;
  while (index < input.length) {
    const char = input[index];
    if (char === ' ' || char === '\t' || char === '\n' || char === '\r' || char === ',') {
      index += 1;
      continue;
    }
    if (char === '#') {
      while (index < input.length && input[index] !== '\n') index += 1;
      continue;
    }
    if (input.startsWith('...', index)) {
      tokens.push({ kind: 'punct', value: '...' });
      index += 3;
      continue;
    }
    if ('{}():$[]!=@'.includes(char)) {
      tokens.push({ kind: 'punct', value: char });
      index += 1;
      continue;
    }
    if (char === '"') {
      let value = '';
      index += 1;
      while (index < input.length && input[index] !== '"') {
        if (input[index] === '\\' && index + 1 < input.length) {
          const escape = input[index + 1];
          if (escape === 'n') value += '\n';
          else if (escape === 't') value += '\t';
          else if (escape === 'r') value += '\r';
          else if (escape === 'u' && index + 5 < input.length) {
            value += String.fromCharCode(Number.parseInt(input.slice(index + 2, index + 6), 16));
            index += 4;
          } else value += escape;
          index += 2;
        } else {
          value += input[index];
          index += 1;
        }
      }
      index += 1;
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (/[0-9-]/.test(char)) {
      const match = /^-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(input.slice(index));
      if (match) {
        tokens.push({ kind: 'number', value: Number(match[0]) });
        index += match[0].length;
        continue;
      }
    }
    if (/[_A-Za-z]/.test(char)) {
      const match = /^[_A-Za-z][_0-9A-Za-z]*/.exec(input.slice(index));
      if (match) {
        tokens.push({ kind: 'name', value: match[0] });
        index += match[0].length;
        continue;
      }
    }
    throw new Error(`unexpected character ${JSON.stringify(char)} in GraphQL document`);
  }
  return tokens;
}

export interface FieldNode {
  name: string;
  alias: string | null;
  args: Record<string, unknown>;
  children: FieldNode[];
  fragmentType: string | null;
}

interface Operation {
  kind: 'query' | 'mutation';
  defaults: Record<string, unknown>;
  selections: FieldNode[];
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const token = this.tokens[this.pos];
    if (!token) throw new Error('unexpected end of GraphQL document');
    this.pos += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) {
      throw new Error(`expected ${JSON.stringify(value)} in GraphQL document`);
    }
  }

  private isPunct(value: string): boolean {
    const token = this.peek();
    return token !== undefined && token.kind === 'punct' && token.value === value;
  }

  parseDocument(): Operation {
    let kind: 'query' | 'mutation' = 'query';
    const first = this.peek();
    if (first?.kind === 'name' && (first.value === 'query' || first.value === 'mutation')) {
      kind = first.value;
      this.next();
      const maybeName = this.peek();
      if (maybeName?.kind === 'name') this.next();
    }
    const defaults: Record<string, unknown> = {};
    if (this.isPunct('(')) {
      this.next();
      while (!this.isPunct(')')) {
        this.expectPunct('$');
        const name = this.next();
        if (name.kind !== 'name') throw new Error('expected a variable name');
        this.expectPunct(':');
        this.skipType();
        if (this.isPunct('=')) {
          this.next();
          defaults[name.value] = this.parseValue();
        }
      }
      this.expectPunct(')');
    }
    const selections = this.parseSelectionSet();
    return { kind, defaults, selections };
  }

  /** Skip a type reference, including list brackets. */
  private skipType(): void {
    if (this.isPunct('[')) {
      this.next();
      this.skipType();
      this.expectPunct(']');
    } else {
      const token = this.next();
      if (token.kind !== 'name') throw new Error('expected a type name');
    }
    if (this.isPunct('!')) this.next();
  }

  private parseSelectionSet(): FieldNode[] {
    this.expectPunct('{');
    const selections: FieldNode[] = [];
    while (!this.isPunct('}')) {
      selections.push(this.parseSelection());
    }
    this.expectPunct('}');
    return selections;
  }

  private parseSelection(): FieldNode {
    if (this.isPunct('...')) {
      this.next();
      const maybeOn = this.peek();
      if (maybeOn?.kind === 'name' && maybeOn.value === 'on') {
        this.next();
        const typeName = this.next();
        if (typeName.kind !== 'name') throw new Error('expected a type name after on');
        return { name: '', alias: null, args: {}, children: this.parseSelectionSet(), fragmentType: typeName.value };
      }
      throw new Error('named fragments are not supported');
    }
    const first = this.next();
    if (first.kind !== 'name') throw new Error('expected a field name');
    let alias: string | null = null;
    let name = first.value;
    if (this.isPunct(':')) {
      this.next();
      const real = this.next();
      if (real.kind !== 'name') throw new Error('expected a field name after an alias');
      alias = name;
      name = real.value;
    }
    const args: Record<string, unknown> = {};
    if (this.isPunct('(')) {
      this.next();
      while (!this.isPunct(')')) {
        const argName = this.next();
        if (argName.kind !== 'name') throw new Error('expected an argument name');
        this.expectPunct(':');
        args[argName.value] = this.parseValue();
      }
      this.expectPunct(')');
    }
    const children = this.peek()?.kind === 'punct' && this.peek()?.value === '{' ? this.parseSelectionSet() : [];
    return { name, alias, args, children, fragmentType: null };
  }

  private parseValue(): unknown {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '$') {
      const name = this.next();
      if (name.kind !== 'name') throw new Error('expected a variable name');
      return { __variable: name.value };
    }
    if (token.kind === 'string' || token.kind === 'number') return token.value;
    if (token.kind === 'punct' && token.value === '[') {
      const values: unknown[] = [];
      while (!(this.peek()?.kind === 'punct' && this.peek()?.value === ']')) values.push(this.parseValue());
      this.next();
      return values;
    }
    if (token.kind === 'punct' && token.value === '{') {
      const object: Record<string, unknown> = {};
      while (!(this.peek()?.kind === 'punct' && this.peek()?.value === '}')) {
        const key = this.next();
        if (key.kind !== 'name') throw new Error('expected an object key');
        this.expectPunct(':');
        object[key.value] = this.parseValue();
      }
      this.next();
      return object;
    }
    if (token.kind === 'name') {
      if (token.value === 'true') return true;
      if (token.value === 'false') return false;
      if (token.value === 'null') return null;
      return token.value;
    }
    throw new Error('unexpected value in GraphQL document');
  }
}

/** Replace `{__variable: name}` markers with the supplied variable values. */
function substituteVariables(value: unknown, variables: Record<string, unknown>): unknown {
  if (Array.isArray(value)) return value.map((item) => substituteVariables(item, variables));
  if (isRecord(value)) {
    if (typeof value.__variable === 'string') return variables[value.__variable];
    const out: Record<string, unknown> = {};
    for (const [key, item] of Object.entries(value)) out[key] = substituteVariables(item, variables);
    return out;
  }
  return value;
}

// ---------------------------------------------------------------------------------------------
// global ids
// ---------------------------------------------------------------------------------------------

export function globalId(kind: string, value: string): string {
  return Buffer.from(`${kind}:${value}`, 'utf8').toString('base64');
}

function decodeGlobalId(id: string): { kind: string; value: string } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const colon = text.indexOf(':');
    if (colon < 0) return null;
    return { kind: text.slice(0, colon), value: text.slice(colon + 1) };
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------------------------

type GqlObject = Record<string, unknown> & { __typename: string };

interface GqlContext {
  store: Store;
}

function asStringValue(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function asNumberValue(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function asStringArrayValue(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) return undefined;
  if (value.some((item) => typeof item !== 'string')) return undefined;
  return value as string[];
}

function cursorForNumber(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function numberFromCursor(cursor: string | undefined): number | undefined {
  if (!cursor) return undefined;
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    if (!text.startsWith('cursor:')) return undefined;
    const value = Number.parseInt(text.slice('cursor:'.length), 10);
    return Number.isFinite(value) ? value : undefined;
  } catch {
    return undefined;
  }
}

interface Page<T> {
  nodes: T[];
  hasNextPage: boolean;
  endCursor: string | null;
}

function paginateByCursor<T>(items: T[], first: number | undefined, after: string | undefined, keyOf: (item: T) => number): Page<T> {
  const afterNumber = numberFromCursor(after);
  let start = 0;
  if (afterNumber !== undefined) {
    const index = items.findIndex((item) => keyOf(item) === afterNumber);
    start = index < 0 ? 0 : index + 1;
  }
  const count = first === undefined ? items.length - start : first;
  const nodes = items.slice(start, start + count);
  return {
    nodes,
    hasNextPage: start + nodes.length < items.length,
    endCursor: nodes.length ? cursorForNumber(keyOf(nodes[nodes.length - 1])) : null,
  };
}

function issueConnection(repo: Repo, issues: Issue[], first: number | undefined, after: string | undefined): GqlObject {
  const page = paginateByCursor(issues, first, after, (issue) => issue.number);
  return {
    __typename: 'IssueConnection',
    nodes: page.nodes.map((issue) => issueObject(repo, issue)),
    edges: page.nodes.map((issue) => ({
      __typename: 'IssueEdge',
      node: issueObject(repo, issue),
      cursor: cursorForNumber(issue.number),
    })),
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: page.hasNextPage,
      endCursor: page.endCursor,
      hasPreviousPage: false,
      startCursor: page.nodes.length ? cursorForNumber(page.nodes[0].number) : null,
    },
    totalCount: issues.length,
  };
}

function pullConnection(repo: Repo, pulls: PullRequest[], first: number | undefined, after: string | undefined): GqlObject {
  const page = paginateByCursor(pulls, first, after, (pull) => pull.number);
  return {
    __typename: 'PullRequestConnection',
    nodes: page.nodes.map((pull) => pullObject(repo, pull)),
    edges: page.nodes.map((pull) => ({
      __typename: 'PullRequestEdge',
      node: pullObject(repo, pull),
      cursor: cursorForNumber(pull.number),
    })),
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: page.hasNextPage,
      endCursor: page.endCursor,
      hasPreviousPage: false,
      startCursor: page.nodes.length ? cursorForNumber(page.nodes[0].number) : null,
    },
    totalCount: pulls.length,
  };
}

function repositoryObject(repo: Repo): GqlObject {
  return { __typename: 'Repository', repo };
}

function issueObject(repo: Repo, issue: Issue): GqlObject {
  return { __typename: 'Issue', repo, issue };
}

function pullObject(repo: Repo, pull: PullRequest): GqlObject {
  return { __typename: 'PullRequest', repo, pull };
}

function commentObject(repo: Repo, comment: IssueComment): GqlObject {
  return { __typename: 'IssueComment', repo, comment };
}

function findRepoByGlobalId(store: Store, id: string): Repo | undefined {
  const decoded = decodeGlobalId(id);
  if (!decoded || decoded.kind !== 'Repository') return undefined;
  const repoId = Number.parseInt(decoded.value, 10);
  if (!Number.isFinite(repoId)) return undefined;
  return [...store.repos.values()].find((repo) => repo.id === repoId);
}

function findIssueByGlobalId(store: Store, id: string): { repo: Repo; issue: Issue } | undefined {
  const decoded = decodeGlobalId(id);
  if (!decoded || decoded.kind !== 'Issue') return undefined;
  const [repoIdText, numberText] = decoded.value.split(':');
  const repoId = Number.parseInt(repoIdText, 10);
  const number = Number.parseInt(numberText, 10);
  if (!Number.isFinite(repoId) || !Number.isFinite(number)) return undefined;
  const repo = [...store.repos.values()].find((item) => item.id === repoId);
  const issue = repo?.issues.find((item) => item.number === number);
  if (!repo || !issue) return undefined;
  return { repo, issue };
}

function findPullByGlobalId(store: Store, id: string): { repo: Repo; pull: PullRequest } | undefined {
  const decoded = decodeGlobalId(id);
  if (!decoded || decoded.kind !== 'PullRequest') return undefined;
  const [repoIdText, numberText] = decoded.value.split(':');
  const repoId = Number.parseInt(repoIdText, 10);
  const number = Number.parseInt(numberText, 10);
  if (!Number.isFinite(repoId) || !Number.isFinite(number)) return undefined;
  const repo = [...store.repos.values()].find((item) => item.id === repoId);
  const pull = repo?.pulls.find((item) => item.number === number);
  if (!repo || !pull) return undefined;
  return { repo, pull };
}

function unknownField(field: FieldNode, typeName: string): Error {
  return new Error(`Cannot query field "${field.name}" on type "${typeName}".`);
}

function resolveField(typeName: string, obj: GqlObject, field: FieldNode, ctx: GqlContext): unknown {
  const store = ctx.store;
  if (field.name === '__typename') return obj.__typename;
  switch (typeName) {
    case 'Query': {
      if (field.name === 'viewer') {
        return { __typename: 'Viewer', login: VIEWER_LOGIN, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL };
      }
      if (field.name === 'repository') {
        const owner = asStringValue(field.args.owner);
        const name = asStringValue(field.args.name);
        if (!owner || !name) return null;
        const repo = store.getRepo(owner, name);
        return repo ? repositoryObject(repo) : null;
      }
      if (field.name === 'node') {
        const id = asStringValue(field.args.id);
        if (!id) return null;
        const decoded = decodeGlobalId(id);
        if (!decoded) return null;
        if (decoded.kind === 'Repository') {
          const repo = findRepoByGlobalId(store, id);
          return repo ? repositoryObject(repo) : null;
        }
        if (decoded.kind === 'Issue') {
          const found = findIssueByGlobalId(store, id);
          return found ? issueObject(found.repo, found.issue) : null;
        }
        if (decoded.kind === 'PullRequest') {
          const found = findPullByGlobalId(store, id);
          return found ? pullObject(found.repo, found.pull) : null;
        }
        if (decoded.kind === 'IssueComment') {
          const parts = decoded.value.split(':');
          const repoId = Number.parseInt(parts[0], 10);
          const commentId = Number.parseInt(parts[1], 10);
          const repo = [...store.repos.values()].find((item) => item.id === repoId);
          const comment = repo?.issues.flatMap((issue) => issue.comments).find((item) => item.id === commentId);
          return repo && comment ? commentObject(repo, comment) : null;
        }
        return null;
      }
      if (field.name === '__type') {
        return typeIntrospection(asStringValue(field.args.name));
      }
      if (field.name === '__schema') {
        return { __typename: '__Schema', queryType: { name: 'Query', kind: 'OBJECT' }, mutationType: { name: 'Mutation', kind: 'OBJECT' } };
      }
      throw unknownField(field, typeName);
    }
    case 'Mutation': {
      if (field.name === 'createIssue') {
        const input = isRecord(field.args.input) ? field.args.input : {};
        const repo = findRepoByGlobalId(store, asStringValue(input.repositoryId) ?? '');
        if (!repo) throw new Error('Repository not found');
        const title = asStringValue(input.title);
        if (!title) throw new Error('title is required');
        const issue = store.createIssue(repo, { title, body: asStringValue(input.body) });
        return { __typename: 'CreateIssuePayload', issue: issueObject(repo, issue) };
      }
      if (field.name === 'closeIssue') {
        const input = isRecord(field.args.input) ? field.args.input : {};
        const found = findIssueByGlobalId(store, asStringValue(input.issueId) ?? '');
        if (!found) throw new Error('Issue not found');
        found.issue.state = 'closed';
        found.issue.updatedAt = FIXED_TIMESTAMP;
        return { __typename: 'CloseIssuePayload', issue: issueObject(found.repo, found.issue) };
      }
      if (field.name === 'createPullRequest') {
        const input = isRecord(field.args.input) ? field.args.input : {};
        const repo = findRepoByGlobalId(store, asStringValue(input.repositoryId) ?? '');
        if (!repo) throw new Error('Repository not found');
        const headRef = asStringValue(input.headRefName);
        const baseRef = asStringValue(input.baseRefName);
        const title = asStringValue(input.title);
        if (!headRef || !baseRef || !title) throw new Error('headRefName, baseRefName and title are required');
        const headSha = repo.refs.get(`refs/heads/${headRef}`);
        const baseSha = repo.refs.get(`refs/heads/${baseRef}`);
        if (!headSha || !baseSha) throw new Error('head or base branch does not exist');
        const duplicate = repo.pulls.find((pull) => pull.state === 'open' && pull.headRef === headRef && pull.baseRef === baseRef);
        if (duplicate) return { __typename: 'CreatePullRequestPayload', pullRequest: pullObject(repo, duplicate) };
        const pull = store.createPull(repo, { title, body: asStringValue(input.body), headRef, baseRef, headSha, baseSha });
        return { __typename: 'CreatePullRequestPayload', pullRequest: pullObject(repo, pull) };
      }
      if (field.name === 'mergePullRequest') {
        const input = isRecord(field.args.input) ? field.args.input : {};
        const found = findPullByGlobalId(store, asStringValue(input.pullRequestId) ?? '');
        if (!found) throw new Error('Pull request not found');
        if (found.pull.state === 'closed') throw new Error('Pull request is already closed');
        const merged = squashMergePull(store, found.repo, found.pull);
        if (!merged) throw new Error('Merge conflict');
        return { __typename: 'MergePullRequestPayload', pullRequest: pullObject(found.repo, found.pull) };
      }
      if (field.name === 'addComment') {
        const input = isRecord(field.args.input) ? field.args.input : {};
        const found = findIssueByGlobalId(store, asStringValue(input.subjectId) ?? '');
        if (!found) throw new Error('Issue not found');
        const body = asStringValue(input.body);
        if (!body) throw new Error('body is required');
        const comment = store.addComment(found.issue, body);
        return {
          __typename: 'AddCommentPayload',
          commentEdge: {
            __typename: 'IssueCommentEdge',
            node: commentObject(found.repo, comment),
            cursor: cursorForNumber(comment.id),
          },
        };
      }
      throw unknownField(field, typeName);
    }
    case 'Viewer': {
      if (field.name === 'login') return obj.login;
      if (field.name === 'id') return globalId('User', String(VIEWER_ID));
      if (field.name === 'name') return obj.name;
      if (field.name === 'email') return obj.email;
      throw unknownField(field, typeName);
    }
    case 'Repository': {
      const repo = obj.repo as Repo;
      if (field.name === 'id') return globalId('Repository', String(repo.id));
      if (field.name === 'databaseId') return repo.id;
      if (field.name === 'name') return repo.name;
      if (field.name === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
      if (field.name === 'description') return repo.description;
      if (field.name === 'url') return `https://github.com/${repo.owner}/${repo.name}`;
      if (field.name === 'createdAt') return repo.createdAt;
      if (field.name === 'updatedAt') return repo.updatedAt;
      if (field.name === 'isPrivate') return repo.isPrivate;
      if (field.name === 'owner') {
        return { __typename: 'User', login: repo.owner, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL };
      }
      if (field.name === 'defaultBranchRef') {
        const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
        return tip ? { __typename: 'Ref', name: repo.defaultBranch } : null;
      }
      if (field.name === 'viewerPermission') return 'ADMIN';
      if (field.name === 'hasIssuesEnabled') return true;
      if (field.name === 'issue') {
        const number = asNumberValue(field.args.number);
        if (number === undefined) return null;
        const issue = repo.issues.find((item) => item.number === number);
        return issue ? issueObject(repo, issue) : null;
      }
      if (field.name === 'issues') {
        let issues = [...repo.issues];
        const states = asStringArrayValue(field.args.states);
        if (states && states.length) {
          const wanted = new Set(states.map((state) => state.toLowerCase()));
          issues = issues.filter((issue) => wanted.has(issue.state));
        }
        const orderBy = isRecord(field.args.orderBy) ? field.args.orderBy : undefined;
        const direction = orderBy && asStringValue(orderBy.direction) === 'DESC' ? -1 : 1;
        issues.sort((a, b) => (a.number - b.number) * direction);
        return issueConnection(repo, issues, asNumberValue(field.args.first), asStringValue(field.args.after));
      }
      if (field.name === 'pullRequest') {
        const number = asNumberValue(field.args.number);
        if (number === undefined) return null;
        const pull = repo.pulls.find((item) => item.number === number);
        return pull ? pullObject(repo, pull) : null;
      }
      if (field.name === 'pullRequests') {
        let pulls = [...repo.pulls];
        const states = asStringArrayValue(field.args.states);
        if (states && states.length) {
          const wanted = new Set(states.map((state) => state.toUpperCase()));
          pulls = pulls.filter((pull) => wanted.has(pullGraphqlState(pull)));
        }
        pulls.sort((a, b) => a.number - b.number);
        return pullConnection(repo, pulls, asNumberValue(field.args.first), asStringValue(field.args.after));
      }
      throw unknownField(field, typeName);
    }
    case 'Issue': {
      const repo = obj.repo as Repo;
      const issue = obj.issue as Issue;
      if (field.name === 'id') return globalId('Issue', `${repo.id}:${issue.number}`);
      if (field.name === 'databaseId') return issue.id;
      if (field.name === 'number') return issue.number;
      if (field.name === 'title') return issue.title;
      if (field.name === 'body') return issue.body;
      if (field.name === 'state') return issue.state.toUpperCase();
      if (field.name === 'url') return `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`;
      if (field.name === 'createdAt') return issue.createdAt;
      if (field.name === 'updatedAt') return issue.updatedAt;
      if (field.name === 'author') {
        return { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL };
      }
      if (field.name === 'labels') {
        return { __typename: 'LabelConnection', nodes: issue.labels.map((name) => ({ __typename: 'Label', name })) };
      }
      if (field.name === 'comments') {
        const page = paginateByCursor(issue.comments, asNumberValue(field.args.first), asStringValue(field.args.after), (comment) => comment.id);
        return {
          __typename: 'IssueCommentConnection',
          nodes: page.nodes.map((comment) => commentObject(repo, comment)),
          edges: page.nodes.map((comment) => ({
            __typename: 'IssueCommentEdge',
            node: commentObject(repo, comment),
            cursor: cursorForNumber(comment.id),
          })),
          pageInfo: {
            __typename: 'PageInfo',
            hasNextPage: page.hasNextPage,
            endCursor: page.endCursor,
            hasPreviousPage: false,
            startCursor: page.nodes.length ? cursorForNumber(page.nodes[0].id) : null,
          },
          totalCount: issue.comments.length,
        };
      }
      if (field.name === 'repository') return repositoryObject(repo);
      throw unknownField(field, typeName);
    }
    case 'PullRequest': {
      const repo = obj.repo as Repo;
      const pull = obj.pull as PullRequest;
      if (field.name === 'id') return globalId('PullRequest', `${repo.id}:${pull.number}`);
      if (field.name === 'databaseId') return pull.id;
      if (field.name === 'number') return pull.number;
      if (field.name === 'title') return pull.title;
      if (field.name === 'body') return pull.body;
      if (field.name === 'state') return pullGraphqlState(pull);
      if (field.name === 'merged') return pull.merged;
      if (field.name === 'mergedAt') return pull.mergedAt;
      if (field.name === 'headRefName') return pull.headRef;
      if (field.name === 'baseRefName') return pull.baseRef;
      if (field.name === 'mergeable') return pullMergeability(store, repo, pull);
      if (field.name === 'url') return `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`;
      if (field.name === 'createdAt') return pull.createdAt;
      if (field.name === 'updatedAt') return pull.updatedAt;
      if (field.name === 'author') {
        return { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL };
      }
      if (field.name === 'files') {
        const baseObject = repo.objects.get(pull.baseSha);
        const headObject = repo.objects.get(pull.headSha);
        if (!baseObject || baseObject.type !== 'commit' || !headObject || headObject.type !== 'commit') {
          return { __typename: 'FileConnection', nodes: [], edges: [], pageInfo: { __typename: 'PageInfo', hasNextPage: false, endCursor: null, hasPreviousPage: false, startCursor: null }, totalCount: 0 };
        }
        const files = diffTrees(store, repo, parseCommit(baseObject.data).tree, parseCommit(headObject.data).tree);
        const first = asNumberValue(field.args.first);
        const nodes = first === undefined ? files : files.slice(0, first);
        return {
          __typename: 'FileConnection',
          nodes: nodes.map((file) => ({
            __typename: 'ChangedFile',
            path: file.filename,
            additions: file.additions,
            deletions: file.deletions,
            changes: file.changes,
            status: file.status.toUpperCase(),
          })),
          edges: nodes.map((file, index) => ({
            __typename: 'FileEdge',
            node: { __typename: 'ChangedFile', path: file.filename, additions: file.additions, deletions: file.deletions, changes: file.changes, status: file.status.toUpperCase() },
            cursor: cursorForNumber(index + 1),
          })),
          pageInfo: {
            __typename: 'PageInfo',
            hasNextPage: first !== undefined && files.length > first,
            endCursor: nodes.length ? cursorForNumber(nodes.length) : null,
            hasPreviousPage: false,
            startCursor: nodes.length ? cursorForNumber(1) : null,
          },
          totalCount: files.length,
        };
      }
      if (field.name === 'repository') return repositoryObject(repo);
      throw unknownField(field, typeName);
    }
    case 'IssueComment': {
      const repo = obj.repo as Repo;
      const comment = obj.comment as IssueComment;
      if (field.name === 'id') return globalId('IssueComment', `${repo.id}:${comment.id}`);
      if (field.name === 'databaseId') return comment.id;
      if (field.name === 'body') return comment.body;
      if (field.name === 'createdAt') return comment.createdAt;
      if (field.name === 'author') {
        return { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL };
      }
      throw unknownField(field, typeName);
    }
    case 'User': {
      if (field.name === 'login') return obj.login;
      if (field.name === 'id') return globalId('User', String(obj.id ?? VIEWER_ID));
      if (field.name === 'name') return obj.name;
      if (field.name === 'email') return obj.email;
      throw unknownField(field, typeName);
    }
    case 'Ref': {
      if (field.name === 'name') return obj.name;
      throw unknownField(field, typeName);
    }
    case 'Label': {
      if (field.name === 'name') return obj.name;
      throw unknownField(field, typeName);
    }
    case 'ChangedFile': {
      if (field.name === 'path') return obj.path;
      if (field.name === 'additions') return obj.additions;
      if (field.name === 'deletions') return obj.deletions;
      if (field.name === 'changes') return obj.changes;
      if (field.name === 'status') return obj.status;
      throw unknownField(field, typeName);
    }
    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'IssueCommentConnection':
    case 'FileConnection':
    case 'LabelConnection': {
      if (field.name === 'nodes') return obj.nodes;
      if (field.name === 'edges') return obj.edges;
      if (field.name === 'pageInfo') return obj.pageInfo;
      if (field.name === 'totalCount') return obj.totalCount;
      throw unknownField(field, typeName);
    }
    case 'IssueEdge':
    case 'PullRequestEdge':
    case 'IssueCommentEdge':
    case 'FileEdge': {
      if (field.name === 'node') return obj.node;
      if (field.name === 'cursor') return obj.cursor;
      throw unknownField(field, typeName);
    }
    case 'PageInfo': {
      if (field.name === 'hasNextPage') return obj.hasNextPage;
      if (field.name === 'endCursor') return obj.endCursor;
      if (field.name === 'hasPreviousPage') return obj.hasPreviousPage;
      if (field.name === 'startCursor') return obj.startCursor;
      throw unknownField(field, typeName);
    }
    case 'CreateIssuePayload':
    case 'CloseIssuePayload': {
      if (field.name === 'issue') return obj.issue;
      throw unknownField(field, typeName);
    }
    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload': {
      if (field.name === 'pullRequest') return obj.pullRequest;
      throw unknownField(field, typeName);
    }
    case 'AddCommentPayload': {
      if (field.name === 'commentEdge') return obj.commentEdge;
      throw unknownField(field, typeName);
    }
    case '__Type': {
      if (field.name === 'name') return obj.name;
      if (field.name === 'kind') return obj.kind;
      if (field.name === 'description') return obj.description ?? null;
      if (field.name === 'fields') return obj.fields ?? null;
      if (field.name === 'enumValues') return obj.enumValues ?? null;
      throw unknownField(field, typeName);
    }
    case '__Schema': {
      if (field.name === 'queryType') return obj.queryType;
      if (field.name === 'mutationType') return obj.mutationType;
      throw unknownField(field, typeName);
    }
    default:
      throw unknownField(field, typeName);
  }
}

function pullGraphqlState(pull: PullRequest): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'closed' ? 'CLOSED' : 'OPEN';
}

/** Static introspection data for the types the checks query. */
function typeIntrospection(name: string | undefined): GqlObject | null {
  if (!name) return null;
  const fieldList = (names: string[]): { name: string; description: string; args: unknown[]; type: { name: string; kind: string } }[] =>
    names.map((fieldName) => ({ name: fieldName, description: '', args: [], type: { name: 'String', kind: 'SCALAR' } }));
  switch (name) {
    case 'Issue':
      return {
        __typename: '__Type',
        name: 'Issue',
        kind: 'OBJECT',
        description: '',
        fields: fieldList(['id', 'databaseId', 'number', 'title', 'body', 'state', 'url', 'createdAt', 'updatedAt', 'author', 'labels', 'comments', 'repository']),
        enumValues: null,
      };
    case 'PullRequest':
      return {
        __typename: '__Type',
        name: 'PullRequest',
        kind: 'OBJECT',
        description: '',
        fields: fieldList(['id', 'databaseId', 'number', 'title', 'body', 'state', 'merged', 'mergedAt', 'headRefName', 'baseRefName', 'mergeable', 'files', 'url', 'createdAt', 'updatedAt', 'author', 'repository']),
        enumValues: null,
      };
    case 'Repository':
      return {
        __typename: '__Type',
        name: 'Repository',
        kind: 'OBJECT',
        description: '',
        fields: fieldList(['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests', 'description', 'url', 'createdAt', 'updatedAt', 'isPrivate']),
        enumValues: null,
      };
    case 'SearchType':
      return {
        __typename: '__Type',
        name: 'SearchType',
        kind: 'ENUM',
        description: '',
        fields: null,
        enumValues: ['ISSUE', 'REPOSITORY', 'USER'].map((value) => ({ name: value, description: '' })),
      };
    default:
      return null;
  }
}

function isGqlObject(value: unknown): value is GqlObject {
  return isRecord(value) && typeof value.__typename === 'string';
}

function materialize(value: unknown, field: FieldNode, ctx: GqlContext): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((item) => materialize(item, field, ctx));
  if (isGqlObject(value)) {
    return resolveSelections(value.__typename, value, field.children, ctx);
  }
  if (isRecord(value)) {
    if (field.children.length === 0) return value;
    const out: Record<string, unknown> = {};
    for (const child of field.children) {
      if (child.fragmentType) continue;
      const raw = value[child.name];
      out[child.alias ?? child.name] = materialize(raw, child, ctx);
    }
    return out;
  }
  return value;
}

function resolveSelections(typeName: string, obj: GqlObject, selections: FieldNode[], ctx: GqlContext): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.fragmentType) {
      if (selection.fragmentType !== typeName) continue;
      Object.assign(out, resolveSelections(typeName, obj, selection.children, ctx));
      continue;
    }
    const field: FieldNode = selection;
    const value = resolveField(typeName, obj, field, ctx);
    out[selection.alias ?? selection.name] = materialize(value, field, ctx);
  }
  return out;
}

function executeOperation(store: Store, operation: Operation, variables: Record<string, unknown>): Record<string, unknown> {
  const ctx: GqlContext = { store };
  const rootType = operation.kind === 'mutation' ? 'Mutation' : 'Query';
  const root: GqlObject = { __typename: rootType };
  return resolveSelections(rootType, root, operation.selections, ctx);
}

/** Replace `{__variable: name}` markers in every field's arguments, at any depth. */
function substituteSelectionArgs(selections: FieldNode[], variables: Record<string, unknown>): FieldNode[] {
  return selections.map((selection) => ({
    ...selection,
    args: substituteVariables(selection.args, variables) as Record<string, unknown>,
    children: substituteSelectionArgs(selection.children, variables),
  }));
}

function executeDocument(store: Store, query: string, variables: Record<string, unknown>): { data: unknown; errors?: { message: string }[] } {
  try {
    const operation = new Parser(tokenize(query)).parseDocument();
    const mergedVariables = { ...operation.defaults, ...variables };
    const selections = substituteSelectionArgs(operation.selections, mergedVariables);
    const data = executeOperation(store, { ...operation, selections }, mergedVariables);
    return { data };
  } catch (error) {
    return { data: null, errors: [{ message: error instanceof Error ? error.message : 'GraphQL execution failed' }] };
  }
}

/**
 * Handle POST /api/graphql. A JSON array is a batch: each entry is executed in
 * order and the results are returned in order.
 */
export function handleGraphql(store: Store, ctx: Ctx, res: ServerResponse): boolean {
  if (ctx.method !== 'POST') return false;
  const parsed = parseJsonBody(ctx.body);
  if (!parsed.ok) {
    sendJson(res, 400, { message: parsed.error }, ctx.requestId);
    return true;
  }
  const requests = Array.isArray(parsed.value) ? parsed.value : [parsed.value];
  const results = requests.map((request) => {
    if (!isRecord(request) || typeof request.query !== 'string') {
      return { data: null, errors: [{ message: 'GraphQL request must include a query string' }] };
    }
    const variables = isRecord(request.variables) ? (request.variables as Record<string, unknown>) : {};
    return executeDocument(store, request.query, variables);
  });
  sendJson(res, 200, Array.isArray(parsed.value) ? results : results[0], ctx.requestId);
  return true;
}
