// Unit tests for the llms-txt plugin: the rendered index names the start-here
// pages with absolute public URLs, and the build contract (not empty, no
// private host, required pages present) throws rather than shipping a bad file.
// Run with `node --test` (npm test, which `npm run build` runs first).

const test = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { render, check, firstSentence } = require('./index');

const PUBLIC = 'https://stump-wtf.github.io/harness/';

function fixture(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'llms-txt-'));
  for (const [rel, body] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, rel)), { recursive: true });
    fs.writeFileSync(path.join(root, rel), body);
  }
  return root;
}

const page = (title, pos, body) => `---\ntitle: "${title}"\nsidebar_position: ${pos}\n---\n\n# ${title}\n\n${body}\n`;

test('lists start-here pages first, with absolute public URLs and a sentence', () => {
  const docsRoot = fixture({
    'guides/onboarding.md': page('Onboarding', 2, 'Set the stack up one layer at a time. More text.'),
    'guides/glossary.md': page('Glossary', 9, 'Every term in one place.'),
    'guides/install.md': page('Install', 1, ':::note\nskip me\n:::\n\nPut [harness](./x) on your `PATH`. Then more.'),
    'usage/cli.md': page('CLI', 1, 'Every verb.'),
  });
  const { llms, full } = render({
    docsRoot, publicUrl: PUBLIC, title: 'Harness', summary: 'systemctl for your agents',
    startHere: ['guides/onboarding', 'guides/glossary'],
  });
  const lines = llms.split('\n');
  assert.strictEqual(lines[0], '# Harness');
  assert.ok(lines.includes('> systemctl for your agents'));
  const start = lines.indexOf('## Start here');
  assert.strictEqual(lines[start + 2], `- [Onboarding](${PUBLIC}guides/onboarding): Set the stack up one layer at a time.`);
  assert.strictEqual(lines[start + 3], `- [Glossary](${PUBLIC}guides/glossary): Every term in one place.`);
  // Admonitions are skipped and links flattened in the description.
  assert.ok(llms.includes(`- [Install](${PUBLIC}guides/install): Put harness on your PATH.`));
  assert.ok(llms.includes(`- [CLI](${PUBLIC}usage/cli): Every verb.`));
  // A start-here page is not listed twice.
  assert.strictEqual(llms.split('guides/onboarding)').length - 1, 1);
  assert.ok(full.includes('Set the stack up one layer at a time.'));
});

test('a missing start-here page fails the build', () => {
  const docsRoot = fixture({ 'guides/install.md': page('Install', 1, 'Install it.') });
  assert.throws(
    () => render({ docsRoot, publicUrl: PUBLIC, title: 'H', summary: 's', startHere: ['guides/onboarding'] }),
    /required page\(s\) not found: guides\/onboarding/,
  );
});

test('an empty index fails the build', () => {
  assert.throws(() => check('llms.txt', '# Harness\n\n> s\n'), /would be empty/);
});

test('a private host fails the build', () => {
  const text = `# H\n\n- [X](${PUBLIC}guides/x): see https://gitea.stump.rocks/stump.wtf/harness.`;
  assert.throws(() => check('llms.txt', text), /private host/);
  const pages = `# H\n\n- [X](https://stump-wtf.pages.stump.rocks/harness/guides/x): y.`;
  assert.throws(() => check('llms.txt', pages), /private host/);
});

test('a required URL missing from the output fails the build', () => {
  const text = `# H\n\n- [X](${PUBLIC}guides/x): y.`;
  assert.throws(() => check('llms.txt', text, { requiredUrls: [`${PUBLIC}guides/glossary`] }), /missing/);
});

test('firstSentence skips code fences, tables and lists', () => {
  const body = '# T\n\n```sh\nnot this.\n```\n\n| a | b |\n|---|---|\n\n- nor this.\n\nThis one. Not this.';
  assert.strictEqual(firstSentence(body), 'This one.');
});
