// Reads back what the ingress service wrote, from an independent connection to
// the same on-disk database. This is the check that a 200 meant something: if
// the rows are not here, the acknowledgement was a lie.
import { PGlite } from "@electric-sql/pglite";
import { DATA_DIR } from "./serve.mjs";

const db = await PGlite.create(DATA_DIR);

const rows = await db.query(`
  SELECT provider_app_id,
         phone_number_id,
         tenant_id IS NOT NULL AS routed,
         signature_verified,
         app_secret_ref,
         octet_length(payload) AS payload_bytes,
         committed_position
    FROM ingress_items
   ORDER BY id`);

console.log(`ingress_items rows: ${rows.rows.length}`);
console.table(rows.rows);

// Prove the stored payloads are byte-exact by re-deriving the HMAC from disk,
// independently of anything the Go process did.
import crypto from "node:crypto";

const { rows: payloads } = await db.query(`
  SELECT payload, signature_header FROM ingress_items ORDER BY id`);

let verified = 0;
for (const r of payloads) {
  const mac = crypto.createHmac("sha256", process.env.META_APP_SECRET ?? "dev-secret-abc");
  mac.update(Buffer.from(r.payload));
  const want = "sha256=" + mac.digest("hex");
  if (want === r.signature_header) verified++;
  else console.log("  MISMATCH", r.signature_header, "vs", want);
}
console.log(`\npayloads whose stored bytes still reproduce their signature: ${verified}/${payloads.length}`);

await db.close();