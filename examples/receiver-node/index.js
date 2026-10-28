// A Node.js receiver for hookpost webhooks, verified with the official
// "standardwebhooks" package. Proves any Standard Webhooks library can
// verify what hookpost sends.
//
//   npm install
//   WEBHOOK_SECRET=whsec_... node index.js

const http = require("node:http");
const { Webhook } = require("standardwebhooks");

const secret = process.env.WEBHOOK_SECRET;
if (!secret) {
  console.error("set WEBHOOK_SECRET to the secret printed by examples/sender");
  process.exit(1);
}
const wh = new Webhook(secret.replace(/^whsec_/, ""));
const port = Number(process.env.PORT || 9000);

http
  .createServer((req, res) => {
    if (req.method !== "POST" || req.url !== "/hooks") {
      res.writeHead(404).end();
      return;
    }
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const body = Buffer.concat(chunks).toString("utf8");
      try {
        const event = wh.verify(body, req.headers); // throws if invalid
        // Deliveries are at-least-once: dedupe on webhook-id.
        console.log(req.headers["webhook-id"], event.type, JSON.stringify(event.data));
        res.writeHead(204).end();
      } catch (err) {
        console.error("rejected:", err.message);
        res.writeHead(401).end();
      }
    });
  })
  .listen(port, () => console.log(`listening on :${port}`));
