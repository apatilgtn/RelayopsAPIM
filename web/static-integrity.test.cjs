// Guards the console bundle against editing accidents that browsers do not
// report: stray control characters (e.g. a "\b" turned into a backspace,
// which silently breaks a regular expression) and calls to helpers that
// do not exist.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const dir = path.join(__dirname, 'static');
const files = fs.readdirSync(dir).filter((f) => /\.(js|css|html)$/.test(f));

test('console sources contain no control characters', () => {
  for (const f of files) {
    const text = fs.readFileSync(path.join(dir, f), 'utf8');
    const bad = [...text.matchAll(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g)];
    assert.strictEqual(bad.length, 0, `${f} contains ${bad.length} control character(s) at offset ${bad[0]?.index}`);
  }
});

test('dialogs are opened through helpers that exist', () => {
  const app = fs.readFileSync(path.join(dir, 'app.js'), 'utf8');
  for (const f of files.filter((f) => f.endsWith('.js'))) {
    const text = fs.readFileSync(path.join(dir, f), 'utf8');
    for (const [, name] of text.matchAll(/\b(\w+(?:Dialog|Modal|Form))\(\{/g)) {
      const defined = new RegExp(`function ${name}\\b|const ${name}\\b|${name}\\s*[,}]`).test(text) || new RegExp(`function ${name}\\b`).test(app);
      assert.ok(defined, `${f} calls ${name}() which is not defined`);
    }
  }
});
