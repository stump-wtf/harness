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
// and ../docs/openspec/specs beside it. A domain carrying both spec.md and
// design.md is emitted as a category directory, so its pages land at
// /specs/<domain>/spec; a domain carrying only one of the two is emitted as
// the single flat page /specs/<domain>.
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
  // Titles Mermaid cannot take verbatim in a quoted label: double quotes, and a
  // leading backtick (plus a `#name;` that must not decode as an entity). Both
  // relate to ADR-0001, so every ADR page and the ADR index draw their nodes.
  fs.writeFileSync(
    path.join(adrs, 'ADR-0002-quoted.md'),
    '---\nstatus: accepted\ndate: 2026-01-01\nrelated: [ADR-0001]\n---\n\n# ADR-0002 — Profiles ("configurations of harnesses")\n\n## Context\n\nSomething.\n'
  );
  fs.writeFileSync(
    path.join(adrs, 'ADR-0003-backtick.md'),
    '---\nstatus: accepted\ndate: 2026-01-01\nrelated: [ADR-0001]\n---\n\n# ADR-0003: `prompt_file` — issue #42; source\n\n## Context\n\nSomething.\n'
  );

  const body = (id, title, text) =>
    `---\nstatus: active\ndate: 2026-01-01\n---\n\n# ${id}: ${title}\n\n## Overview\n\n${text}\n`;

  // `design: false` leaves the domain with a spec.md only.
  const domain = (name, id, title, text, { design = true } = {}) => {
    const dir = path.join(root, 'docs/openspec/specs', name);
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, 'spec.md'), body(id, title, text));
    if (design) fs.writeFileSync(path.join(dir, 'design.md'), body(id, `${title} Design`, text));
  };

  domain('alpha', 'SPEC-0001', 'Alpha', 'Alpha stands alone.');
  // A spec.md converted from an ADR whose H1 was never renumbered. Its ID must
  // not be registered in the spec mapping: transformSpecReferences runs first,
  // so it would capture every ADR-0001 mention on the site.
  domain('stale', 'ADR-0001', 'Stale Conversion', 'Converted, never renumbered.');
  domain('beta', 'SPEC-0002', 'Beta', 'Beta builds on SPEC-0001.');
  // The prose here mentions `### Requirement:` and then cites an ADR on the
  // same line — the shape that used to register "ADR" as a spec prefix.
  domain(
    'gamma',
    'SPEC-0003',
    'Gamma',
    'Gamma needs SPEC-0001, SPEC-0002 and SPEC-0004.\n\nOne issue per ### Requirement: section, per ADR-0001.'
  );
  // Only a spec.md, no design.md — this domain renders as the flat page
  // /specs/delta, so references to SPEC-0004 must not be sent to
  // /specs/delta/spec, which nothing writes.
  domain('delta', 'SPEC-0004', 'Delta', 'Delta stands alone.', { design: false });
  // Every shape the linkifier must leave alone, alongside bare mentions of the
  // same IDs on the same lines that it must still resolve.
  domain(
    'epsilon',
    'SPEC-0005',
    'Epsilon',
    [
      'Epsilon cites [SPEC-0002](/specs/beta/spec) and [ADR-0001](../../adrs/ADR-0001-example.md).',
      '',
      'The literals `SPEC-0002` and `ADR-0001` are prose, not references.',
      '',
      'Prior art: <a href="/harness/specs/beta/spec" className="rfc-ref">SPEC-0002</a> covers it.',
      '',
      'Prior art: <a href="/harness/decisions/ADR-0001-example" className="rfc-ref">ADR-0001</a> covers it.',
      '',
      'Compare `SPEC-0002` against bare SPEC-0001, and `ADR-0001` against bare ADR-0001.',
    ].join('\n')
  );

  return { root, site };
}

// Takes the test context so the fixture is torn down via t.after(), which runs
// even when an assertion throws. A trailing rmSync would leak the tmpdir on
// exactly the runs worth re-reading.
async function build(t) {
  const { root, site } = writeFixture();
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  await plugin({ siteDir: site, siteConfig: { baseUrl: '/harness/', title: 'Harness' } }, {}).loadContent();
  const read = (rel) => fs.readFileSync(path.join(root, 'docs-generated', rel), 'utf-8');
  const generated = (rel) => path.join(root, 'docs-generated', rel);
  return { read, generated };
}

test('every SPEC reference resolves to its own spec directory', async (t) => {
  const { read } = await build(t);

  const beta = read('specs/beta/spec.mdx');
  assert.match(beta, /href="\/harness\/specs\/alpha\/spec"/);

  const gamma = read('specs/gamma/spec.mdx');
  assert.match(gamma, /href="\/harness\/specs\/alpha\/spec"/);
  assert.match(gamma, /href="\/harness\/specs\/beta\/spec"/);

  // The regression: with the artifact ID keyed by prefix, all of these pointed
  // at whichever domain was read last.
  assert.doesNotMatch(gamma, /href="[^"]*\/specs\/gamma\/spec"[^>]*>SPEC-000[12]</);
});

test('artifact references carry no fragment', async (t) => {
  const { read } = await build(t);

  // A spec page's H1 anchor is derived from the whole heading text
  // ("spec-0001-alpha"), so `#spec-0001` pointed at nothing.
  assert.doesNotMatch(read('specs/beta/spec.mdx'), /#spec-0001/);
});

test('a spec H1 carrying an ADR number does not claim that ID', async (t) => {
  const { read } = await build(t);

  const gamma = read('specs/gamma/spec.mdx');
  // Still the ADR page, and no spec route claims the ID. Registering it
  // produced `<a href="/specs/stale/spec"><a href="/decisions/...">ADR-0001</a></a>`,
  // so the nested-anchor shape is the assertion that actually bites.
  assert.match(gamma, /href="\/harness\/decisions\/ADR-0001-example"/);
  assert.doesNotMatch(gamma, /href="[^"]*\/specs\/stale/);
  assert.doesNotMatch(gamma, /<a [^>]*><a /);
});

test('a spec citing an ADR does not claim the ADR prefix', async (t) => {
  const { read } = await build(t);

  const gamma = read('specs/gamma/spec.mdx');
  assert.match(gamma, /href="\/harness\/decisions\/ADR-0001-example"/);
  // With "ADR" registered as a spec prefix the spec transform got there first
  // and wrapped the ADR link in a second anchor pointing at a spec page.
  assert.doesNotMatch(gamma, /href="[^"]*#adr-0001"/);
  assert.doesNotMatch(gamma, /<a [^>]*><a /);
});

test('a design-less domain is referenced at its flat page', async (t) => {
  const { read, generated } = await build(t);

  // What the transform actually emits for a spec.md-only domain.
  assert.ok(fs.existsSync(generated('specs/delta.mdx')));
  assert.ok(!fs.existsSync(generated('specs/delta/spec.mdx')));

  // The regression: the mapping assumed every domain was nested, so this
  // cross-reference pointed at /specs/delta/spec — a route with no page.
  const gamma = read('specs/gamma/spec.mdx');
  assert.match(gamma, /href="\/harness\/specs\/delta"[^>]*>SPEC-0004</);
  assert.doesNotMatch(gamma, /href="[^"]*\/specs\/delta\/spec"/);

  // The specs index links the same page the transform wrote.
  const index = read('specs/index.mdx');
  assert.match(index, /\[Specification\]\(\.\/delta\)/);
  assert.doesNotMatch(index, /\(\.\/delta\/spec\)/);
});

// --- Already-linked references stay untouched --------------------------------
//
// Linkifying a reference that is already inside inline code, a markdown link,
// or an anchor an earlier pass emitted produces `<a><a>...</a></a>`. That is
// invalid HTML, and the SSG's minifier rejects it rather than repairing it.

test('a reference inside inline code is left alone', async (t) => {
  const { read } = await build(t);

  const epsilon = read('specs/epsilon/spec.mdx');
  assert.match(epsilon, /`SPEC-0002`/);
  assert.match(epsilon, /`ADR-0001`/);
});

test('a reference inside a markdown link is left alone', async (t) => {
  const { read } = await build(t);

  const epsilon = read('specs/epsilon/spec.mdx');
  // The label keeps its original text; only the .md suffix is stripped.
  assert.match(epsilon, /\[SPEC-0002\]\(\/specs\/beta\/spec\)/);
  assert.match(epsilon, /\[ADR-0001\]\(\.\.\/\.\.\/adrs\/ADR-0001-example\)/);
});

test('a reference inside an emitted anchor is left alone', async (t) => {
  const { read } = await build(t);

  const epsilon = read('specs/epsilon/spec.mdx');
  // Both halves of the anchor matter. `<[^>]+>` alone ranges the bare tag, so
  // it covers an ID sitting in the href but not one sitting in the link text —
  // and the link text is the half that actually nests.
  assert.doesNotMatch(epsilon, /<a [^>]*><a /);
  assert.match(
    epsilon,
    /Prior art: <a href="\/harness\/specs\/beta\/spec" className="rfc-ref">SPEC-0002<\/a> covers it\./
  );
  assert.match(
    epsilon,
    /Prior art: <a href="\/harness\/decisions\/ADR-0001-example" className="rfc-ref">[^<]*ADR-0001<\/a> covers it\./
  );
});

test('an unprotected reference on the same line still linkifies', async (t) => {
  const { read } = await build(t);

  // The guard is span-scoped, not line-scoped: one protected span must not
  // suppress a bare mention elsewhere on the same line.
  const epsilon = read('specs/epsilon/spec.mdx');
  assert.match(epsilon, /`SPEC-0002` against bare <a href="\/harness\/specs\/alpha\/spec"[^>]*>SPEC-0001</);
  assert.match(epsilon, /`ADR-0001` against bare <a href="\/harness\/decisions\/ADR-0001-example"[^>]*>[^<]*ADR-0001</);
});

// --- Mermaid node labels -----------------------------------------------------
//
// A node label is a quoted Mermaid string, and a title dropped into it verbatim
// can break the whole diagram: a double quote closes it early ("Parse error ...
// Expecting 'SQE', got 'STR'"), and a leading backtick opens a markdown string
// ("Lexical error ... Unrecognized text"). ADR-0006 and ADR-0018 hit one each,
// and each took down the graph on every page that drew its node.

const mermaidBlocks = (mdx) =>
  [...mdx.matchAll(/```mermaid\n([\s\S]*?)```/g)].map((m) => m[1]);

// Every node definition, `  ID["label"]`, captured up to the closing `"]`.
const nodeLabels = (block) =>
  [...block.matchAll(/^\s+\w+\["(.*)"\]\s*$/gm)].map((m) => m[1]);

test('titles Mermaid cannot quote verbatim are entity-escaped in node labels', async (t) => {
  const { read } = await build(t);

  const quoted = 'ADR-0002 — Profiles (#quot;configurations of harnesses#quot;)';
  const backtick = '#96;prompt_file#96; — issue #35;42; source';

  // A mini-DAG draws the page's direct neighbours; the ADR index draws the
  // hierarchy graph of every ADR.
  const pages = {
    'decisions/ADR-0001-example.mdx': [quoted, backtick],
    'decisions/ADR-0002-quoted.mdx': [quoted],
    'decisions/ADR-0003-backtick.mdx': [backtick],
    'decisions/index.mdx': [quoted, backtick],
  };

  for (const [rel, want] of Object.entries(pages)) {
    const blocks = mermaidBlocks(read(rel));
    assert.ok(blocks.length > 0, `${rel} has no mermaid block`);
    const labels = blocks.flatMap(nodeLabels);
    for (const label of want) assert.ok(labels.includes(label), `${rel}: no node labelled ${label}`);
    for (const label of labels) assert.doesNotMatch(label, /["`]/, `${rel}: unescaped ${label}`);
  }
});
