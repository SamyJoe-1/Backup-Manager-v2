// Drives headless Chromium over the DevTools protocol: signs in, opens every page, clicks the main
// actions, records JavaScript errors and takes screenshots.
const base = "http://127.0.0.1:18750", out = process.argv[2];
import { writeFileSync } from "node:fs";
const tab = await (await fetch("http://127.0.0.1:9333/json/new?about:blank", { method: "PUT" })).json();
const ws = new WebSocket(tab.webSocketDebuggerUrl); await new Promise(r => ws.onopen = r);
let id = 0; const wait = new Map(), errors = [];
ws.onmessage = m => { const d = JSON.parse(m.data);
  if (d.id && wait.has(d.id)) { wait.get(d.id)(d.result || d); wait.delete(d.id); }
  if (d.method === "Runtime.exceptionThrown") errors.push("EXCEPTION " + (d.params.exceptionDetails.exception?.description || d.params.exceptionDetails.text));
  if (d.method === "Runtime.consoleAPICalled" && d.params.type === "error") errors.push("console.error " + d.params.args.map(a => a.value || a.description).join(" "));
  if (d.method === "Log.entryAdded" && d.params.entry.level === "error") errors.push("LOG " + d.params.entry.text + " " + (d.params.entry.url || "")); };
const send = (method, params = {}) => new Promise(r => { wait.set(++id, r); ws.send(JSON.stringify({ id, method, params })); });
const sleep = ms => new Promise(r => setTimeout(r, ms));
const js = async expr => { const r = await send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true }); if (r.exceptionDetails) errors.push("EVAL " + expr.slice(0, 60) + " -> " + (r.exceptionDetails.exception?.description || r.exceptionDetails.text)); return r.result?.value; };
const shot = async name => { const r = await send("Page.captureScreenshot", { format: "png" }); writeFileSync(`${out}/${name}.png`, Buffer.from(r.data, "base64")); };
const step = async (name, fn) => { const n = errors.length; await fn(); await sleep(900); await shot(name); const t = await js("document.querySelector('.toast .err')?.textContent || ''"); if (t) errors.push(`TOAST on ${name}: ${t}`); console.log((errors.length > n ? "FAIL " : "ok   ") + name); };
await send("Runtime.enable"); await send("Log.enable"); await send("Page.enable");
await send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });

await step("01-signin", async () => { await send("Page.navigate", { url: base + "/" }); await sleep(800); });
await step("02-wrong-password", async () => { await js(`document.querySelector('[name=email]').value='owner@example.com';document.querySelector('[name=password]').value='nope-nope-nope';document.querySelector('#authForm button').click()`); await sleep(600); await js("document.querySelectorAll('.toast div').forEach(d=>d.remove())"); errors.length = 0; /* the wrong-password toast and its 401 are expected */ });
if (!(await js("!!document.querySelector('#authForm')"))) errors.push("wrong password did not keep us on the sign-in form");
await step("03-dashboard", async () => { await js(`document.querySelector('[name=password]').value='correct-horse-9';document.querySelector('#authForm button').click()`); await sleep(1200); });
if (!(await js("!!document.querySelector('.tiles')"))) errors.push("dashboard tiles missing after sign-in");
for (const [i, r] of ["backups","plans","targets","destinations","jobs","settings","audit"].entries())
  await step(`0${i + 4}-${r}`, async () => { await js(`location.hash='#/${r}'`); });
await step("11-backup-detail", async () => { await js(`location.hash='#/backups'`); await sleep(900); await js(`[...document.querySelectorAll('tr.click')].find(t=>t.querySelector('.pill.done')).click()`); });
const tablesShown = await js("document.querySelectorAll('.drawer #tl tbody tr').length"); if (!tablesShown) errors.push("backup detail shows no tables");
await step("12-table-structure", async () => { await js(`document.querySelector('.drawer [data-ddl]').click()`); });
if (!(await js("(document.querySelector('.modal pre')?.textContent || '').includes('CREATE TABLE')"))) errors.push("structure dialog has no CREATE TABLE");
await step("13-backups-filtered", async () => { await js(`document.querySelectorAll('.ov').forEach(o=>o.remove()); const s=document.querySelector('#fs'); s.value='failed'; s.dispatchEvent(new Event('change'))`); });
const failedRows = await js("[...document.querySelectorAll('#list tbody tr .pill')].map(p=>p.textContent).join(',')"); if (!/^failed/.test(failedRows)) errors.push("status filter shows: " + failedRows);
await step("14-plan-form-tables", async () => { await js(`location.hash='#/plans'`); await sleep(900); await js(`document.querySelectorAll('[data-edit]')[1].click()`); await sleep(1500); });
const picked = await js("document.querySelectorAll('#pick input:checked').length"); if (picked !== 3) errors.push("plan form should show 3 chosen tables, shows " + picked);
await step("15-new-plan", async () => { await js(`document.querySelectorAll('.ov').forEach(o=>o.remove()); document.querySelector('#add').click()`); await sleep(500);
  await js(`const f=document.querySelector('#pf'); f.name.value='Made in the panel'; f.querySelector('[name=scope][value=dbs]').click()`); await sleep(1500);
  await js(`const c=[...document.querySelectorAll('#pick input')].find(i=>i.value==='other'); c.click()`); await shot("15a-new-plan-filled");
  await js(`document.querySelector('#pf button.pri').click()`); await sleep(900); });
if (!(await js("document.body.textContent.includes('Made in the panel')"))) errors.push("new plan was not saved from the form");
await step("16-run-now-live-progress", async () => { await js(`[...document.querySelectorAll('tr')].find(t=>t.textContent.includes('Slow demo')).querySelector('[data-run]').click()`); await sleep(5000); });
const live = await js("document.querySelector('[data-prog]')?.textContent || ''"); if (!/rows/.test(live)) errors.push("no live progress on the running backup: '" + live + "'");
await step("17-jobs-running", async () => { await js(`location.hash='#/jobs'`); await sleep(1500); });
await step("18-cancel", async () => { await js(`document.querySelector('[data-cancel]').click()`); await sleep(2500); });
if (!(await js("document.body.textContent.includes('cancelled')"))) errors.push("cancel from the panel did not show a cancelled job");
await step("19-add-database-form", async () => { await js(`location.hash='#/targets'`); await sleep(800); await js(`document.querySelector('#add').click()`); });
await step("20-test-connection", async () => { await js(`document.querySelectorAll('.ov').forEach(o=>o.remove()); document.querySelector('[data-test]').click()`); await sleep(1500); });
if (!/OK/.test(await js("document.querySelector('[data-res]').textContent"))) errors.push("test connection did not report OK");
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await step("21-phone-backups", async () => { await js(`location.hash='#/backups'`); });
await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "dark" }] });
await send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });
await step("22-dark-dashboard", async () => { await js(`location.hash='#/dashboard'`); });
console.log(errors.length ? "\nERRORS:\n" + errors.join("\n") : "\nno JavaScript errors");
ws.close(); process.exit(errors.length ? 1 : 0);
