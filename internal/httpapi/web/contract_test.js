// Checks the live page's client contract (docs/decisions/020) with a fake
// DOM, WebSocket and fetch. Run: make test-page (needs Node).
const fs = require("fs");
const html = fs.readFileSync(require("path").join(__dirname, "index.html"), "utf8");
const src = html.slice(html.indexOf("<script>") + 8, html.indexOf("</script>"));
const els = {}; const el = (id) => (els[id] ??= { textContent: "", value: "" });
const document = { getElementById: el };
let sockets = [], fetchResolvers = [], fetchLog = [];
class WebSocket { constructor(u) { this.url = u; sockets.push(this); } close() {} static CLOSED = 3; }
const location = { protocol: "http:", host: "x" };
const fetch = (url) => { fetchLog.push(url); return new Promise((res) => fetchResolvers.push({ url, res })); };
const setTimeout = () => {};
const crypto = { randomUUID: () => "u" };
eval(src + ";globalThis.api={connect,getHead:()=>head,getAuction:()=>auction,isResyncing:()=>resyncing};");
const a = (id, head, price) => ({ id, current_bid_id: head, current_price: price, current_leader_id: 1, minimum_bid: 1, min_increment: 100, starting_price: 1000 });
const send = (ws, m) => ws.onmessage({ data: JSON.stringify(m) });
const tick = () => new Promise((r) => globalThis.setImmediate ? setImmediate(r) : r());
let fails = 0; const check = (name, cond) => { console.log((cond ? "PASS " : "FAIL ") + name); if (!cond) fails++; };

(async () => {
  // Watch auction 1.
  el("auction").value = "1"; api.connect(); let ws = sockets.at(-1);
  send(ws, { type: "snapshot", auction: a(1, 10, 1000) });
  send(ws, { type: "bid", bid: { id: 11, auction_id: 1, user_id: 2, amount: 1100, prev_bid_id: 10 } });
  check("bid extending head is applied", api.getHead() === 11);
  send(ws, { type: "sync", auction_id: 1, head_bid_id: 10 });
  check("older sync (overtaken by bid) does not trigger resync", fetchLog.length === 0);
  send(ws, { type: "bid", bid: { id: 13, auction_id: 1, user_id: 2, amount: 1300, prev_bid_id: 12 } });
  check("gap triggers exactly one resync", fetchLog.length === 1);
  send(ws, { type: "sync", auction_id: 1, head_bid_id: 13 });
  check("no second resync while one is in flight", fetchLog.length === 1);
  fetchResolvers[0].res({ ok: true, json: async () => a(1, 9, 900) }); await tick(); await tick(); await tick();
  check("stale resync answer (older head) is ignored", api.getHead() === 11);
  send(ws, { type: "sync", auction_id: 2, head_bid_id: 99 });
  check("sync for another auction is ignored", fetchLog.length === 1);

  // Switch to auction 2 while a resync for auction 1 is pending.
  send(ws, { type: "bid", bid: { id: 20, auction_id: 1, user_id: 2, amount: 2000, prev_bid_id: 18 } });
  const pending = fetchResolvers.at(-1);
  el("auction").value = "2"; api.connect(); ws = sockets.at(-1);
  send(ws, { type: "snapshot", auction: a(2, 50, 5000) });
  pending.res({ ok: true, json: async () => a(1, 20, 2000) }); await tick(); await tick(); await tick();
  check("late answer for the previous auction does not replace the new one", api.getAuction().id === 2 && api.getHead() === 50);
  process.exit(fails ? 1 : 0);
})();
