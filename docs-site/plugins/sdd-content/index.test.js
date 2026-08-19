/**
 * Regression tests for spec and ADR cross-reference resolution.
 *
 * The plugin linkifies a bare `SPEC-0008` or `ADR-0011` mention in an ADR or
 * spec body against a mapping built from the specs tree. That mapping holds two
 * kinds of key, and conflating them is what these tests pin:
 *
 *   "SPEC-0008"  a spec's own artifact ID    -> that spec's page, no fragment
 *   "ARCH"       a domain requirement prefix -> the domain page + #arch-001
 *
 * Keying the artifact ID by its prefix ("SPEC") gave every domain the same key,
 * so the last domain read owned every SPEC-NNNN reference on the site — here,
 * /harness/specs/tui/spec.
 *
 * The plugin is vendored from https://github.com/joestump/claude-plugin-sdd;
 * the same suite lives there over all three copies of the transform.
 *
 * Run with `npm test` from docs-site/, or directly:
 *
 *   node --test docs-site/plugins/sdd-content/index.test.js
 */

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const plugin = require('./index');

// A fixture tree in the layout the plugin expects: siteDir with ../docs/adrs
// and ../docs/openspec/specs beside it. Each domain carries both spec.md and
// design.md, which is what puts its pages at /specs/<domain>/spec.
function writeFixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'sdd-spec-refs-'));
  const site = path.join(root, 'site');
  fs.mkdirSync(site, { recursive: true });

  const adrs = path.join(root, 'docs/adrs');
  fs.mkdirSync(adrs, { recursive: true });
  fs.writeFileSync(
    path.join(adrs, 'ADR-0001-example.md'),
    '---\nstatus: accepted\ndate: 2026-01-01\n---\n\n# ADR-0001: Example\n\n## Context\n\nSomething.\n'
  );

  const body = (id, title, text) =>
    `---\nstatus: active\ndate: 2026-01-01\n---\n\n# ${id}: ${title}\n\n## Overview\n\n${text}\n`;

  const domain = (name, id, title, text) => {
    const dir = path.join(root, 'docs/openspec/specs', name);
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, 'spec.md'), body(id, title, text));
    fs.writeFileSync(path.join(dir, 'design.md'), body(id, `${title} Design`, text));
  };

  domain('alpha', 'SPEC-0001', 'Alpha', 'Alpha stands alone.');
  domain('beta', 'SPEC-0002', 'Beta', 'Beta builds on SPEC-0001.');
  // The prose here mentions `### Requirement:` and then cites an ADR on the
  // same line — the shape that used to register "ADR" as a spec prefix.
  domain(
    'gamma',
    'SPEC-0003',
    'Gamma',
    'Gamma needs SPEC-0001 and SPEC-0002.\n\nOne issue per ### Requirement: section, per ADR-0001.'
  );

  return { root, site };
}

async function build() {
  const { root, site } = writeFixture();
  await plugin({ siteDir: site, siteConfig: { baseUrl: '/harness/', title: 'Harness' } }, {}).loadContent();
  const read = (rel) => fs.readFileSync(path.join(root, 'docs-generated', rel), 'utf-8');
  return { root, read };
}

test('every SPEC reference resolves to its own spec directory', async () => {
  const { root, read } = await build();

  const beta = read('specs/beta/spec.mdx');
  assert.match(beta, /href="\/harness\/specs\/alpha\/spec"/);

  const gamma = read('specs/gamma/spec.mdx');
  assert.match(gamma, /href="\/harness\/specs\/alpha\/spec"/);
  assert.match(gamma, /href="\/harness\/specs\/beta\/spec"/);

  // The regression: with the artifact ID keyed by prefix, all of these pointed
  // at whichever domain was read last.
  assert.doesNotMatch(gamma, /href="[^"]*\/specs\/gamma\/spec"[^>]*>SPEC-000[12]</);

  fs.rmSync(root, { recursive: true, force: true });
});

test('artifact references carry no fragment', async () => {
  const { root, read } = await build();

  // A spec page's H1 anchor is derived from the whole heading text
  // ("spec-0001-alpha"), so `#spec-0001` pointed at nothing.
  assert.doesNotMatch(read('specs/beta/spec.mdx'), /#spec-0001/);

  fs.rmSync(root, { recursive: true, force: true });
});

test('a spec citing an ADR does not claim the ADR prefix', async () => {
  const { root, read } = await build();

  const gamma = read('specs/gamma/spec.mdx');
  assert.match(gamma, /href="\/harness\/decisions\/ADR-0001-example"/);
  // With "ADR" registered as a spec prefix the spec transform got there first
  // and wrapped the ADR link in a second anchor pointing at a spec page.
  assert.doesNotMatch(gamma, /href="[^"]*#adr-0001"/);
  assert.doesNotMatch(gamma, /<a [^>]*><a /);

  fs.rmSync(root, { recursive: true, force: true });
});
