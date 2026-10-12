// Renders the legal and support pages from a local static server and checks
// for layout problems: horizontal overflow, broken internal links, missing headings.
const { chromium } = require('@playwright/test');
(async () => {
  const base = process.env.BASE || 'http://127.0.0.1:18090';
  const browser = await chromium.launch({ channel: 'chrome' });
  let problems = 0;
  for (const [w, h, label] of [[1280, 900, 'desktop'], [390, 844, 'phone']]) {
    const page = await browser.newPage({ viewport: { width: w, height: h } });
    for (const path of ['/legal/terms.html', '/legal/privacy.html', '/support.html']) {
      const res = await page.goto(base + path);
      const info = await page.evaluate(() => ({
        overflow: document.documentElement.scrollWidth > window.innerWidth + 1,
        h1: document.querySelector('h1')?.textContent,
        links: [...document.querySelectorAll('a[href^="/"]')].map(a => a.getAttribute('href')),
      }));
      const broken = [];
      for (const href of [...new Set(info.links)]) {
        const r = await page.request.get(base + href);
        if (r.status() !== 200) broken.push(`${href} ${r.status()}`);
      }
      const bad = res.status() !== 200 || info.overflow || !info.h1 || broken.length;
      if (bad) problems++;
      console.log(`${label} ${path}: ${res.status()} h1="${info.h1}" overflow=${info.overflow} broken=${broken.join(',') || 'none'}`);
      if (path === '/legal/privacy.html') await page.screenshot({ path: `../test-results/privacy-${label}.png`, fullPage: false });
    }
    await page.close();
  }
  await browser.close();
  process.exit(problems ? 1 : 0);
})();
