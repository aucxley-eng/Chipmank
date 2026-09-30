// Applies db/migrations in order to an in-process Postgres, then runs the
// tenant-isolation acceptance tests from Architecture v2 section 39.
//
// Why PGlite: it is real Postgres compiled to WebAssembly, so DDL, domains,
// triggers, plpgsql and row-level security all behave exactly as they do on a
// server. That lets the schema and the isolation proof be verified with no
// Postgres server, no Docker and no cloud account. What it cannot verify is
// concurrency: PGlite is single-connection, so FOR UPDATE SKIP LOCKED and the
// claim/lease functions are exercised for correctness, not for contention.
//
// That concurrency gap is exactly why the schema keeps the uniqueness and
// foreign-key constraints declarative: the two concurrent workers PGlite
// cannot simulate are constrained by the database, not by application code.

import { PGlite } from "@electric-sql/pglite";
import { readdir, readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const migrationsDir = join(here, "..", "..", "db", "migrations");

let passed = 0;
let failed = 0;

function ok(name) {
  passed++;
  console.log(`  PASS  ${name}`);
}
function bad(name, err) {
  failed++;
  console.log(`  FAIL  ${name}\n        ${String(err.message || err).split("\n")[0]}`);
}

// A failing statement must roll the transaction back, or one bad migration
// leaves the schema half-applied and every later error is noise.
async function applyMigration(db, file, sql) {
  await db.transaction(async (tx) => {
    await tx.exec(sql);
  });
  console.log(`  applied ${file}`);
}

async function main() {
  const db = new PGlite();

  console.log("== applying migrations ==");
  const files = (await readdir(migrationsDir))
    .filter((f) => f.endsWith(".sql"))
    .sort();

  for (const file of files) {
    const sql = await readFile(join(migrationsDir, file), "utf8");
    try {
      await applyMigration(db, file, sql);
    } catch (err) {
      console.error(`\nMIGRATION FAILED: ${file}\n${err.message}\n`);
      process.exit(1);
    }
  }

  // ------------------------------------------------------------ fixtures
  console.log("\n== fixtures ==");
  await db.exec(`
    INSERT INTO tenants (id, slug, name) VALUES
      ('11111111-1111-1111-1111-111111111111', 'acme',  'Acme'),
      ('22222222-2222-2222-2222-222222222222', 'globex','Globex');
  `);

  const [ca] = (
    await db.query(`
      INSERT INTO channel_accounts
        (tenant_id, provider_app_id, waba_id, phone_number_id, status)
      VALUES
        ('11111111-1111-1111-1111-111111111111','app-1','waba-1','pn-1','active'),
        ('22222222-2222-2222-2222-222222222222','app-1','waba-2','pn-2','active')
      RETURNING id, tenant_id
    `)
  ).rows;
  const [ca2] = (await db.query(
    `SELECT id FROM channel_accounts WHERE phone_number_id = 'pn-2'`
  )).rows;

  const [c1] = (await db.query(`
    INSERT INTO contacts (tenant_id, display_name) VALUES
      ('11111111-1111-1111-1111-111111111111','Alice')
    RETURNING id
  `)).rows;
  const [c2] = (await db.query(`
    INSERT INTO contacts (tenant_id, display_name) VALUES
      ('22222222-2222-2222-2222-222222222222','Bob')
    RETURNING id
  `)).rows;

  const [i1] = (await db.query(`
    INSERT INTO contact_identities
      (tenant_id, contact_id, channel_account_id, phone_e164)
    VALUES ('11111111-1111-1111-1111-111111111111',$1,$2,'+15550000001')
    RETURNING id
  `, [c1.id, ca.id])).rows;
  const [i2] = (await db.query(`
    INSERT INTO contact_identities
      (tenant_id, contact_id, channel_account_id, phone_e164)
    VALUES ('22222222-2222-2222-2222-222222222222',$1,$2,'+15550000002')
    RETURNING id
  `, [c2.id, ca2.id])).rows;

  await db.query(`
    INSERT INTO tenant_routing_bindings
      (provider_app_id, phone_number_id, tenant_id, channel_account_id)
    SELECT 'app-1', phone_number_id, tenant_id, id FROM channel_accounts
  `);

  // One inbound message per tenant, to prove the projection and ordering paths
  // are tenant-scoped too.
  for (const [tenant, contact, acc, identity] of [
    ["11111111-1111-1111-1111-111111111111", c1, ca, i1],
    ["22222222-2222-2222-2222-222222222222", c2, ca2, i2],
  ]) {
    const [conv] = (await db.query(`
      INSERT INTO conversations
        (tenant_id, channel_account_id, contact_identity_id)
      VALUES ($1,$2,$3) RETURNING id
    `, [tenant, acc.id, identity.id])).rows;

    await db.query(`
      INSERT INTO messages
        (tenant_id, conversation_id, channel_account_id, direction,
         message_type, provider_message_id, aggregate_seq)
      VALUES ($1,$2,$3,'inbound','text',$4,1)
    `, [tenant, conv.id, acc.id, `wamid.${contact.id.slice(0, 8)}`]);
  }
  console.log("  seeded 2 tenants, 2 conversations, 2 messages");

  // ------------------------------------------------------- role guards
  console.log("\n== role privilege guards ==");
  try {
    await db.exec("SET ROLE app_runtime");
    await db.exec("RESET ROLE");
    ok("app_runtime exists and is assumable");
  } catch (e) {
    bad("app_runtime exists and is assumable", e);
  }

  const A = "11111111-1111-1111-1111-111111111111";
  const B = "22222222-2222-2222-2222-222222222222";

  // ------------------------------------------------- isolation: read path
  console.log("\n== tenant isolation: read path ==");

  async function asTenant(role, tenantId, sql, params = []) {
    return db.transaction(async (tx) => {
      await tx.exec(`SET LOCAL ROLE ${role}`);
      if (tenantId) {
        await tx.query(`SELECT set_config('app.tenant_id', $1, true)`, [tenantId]);
      }
      return tx.query(sql, params);
    });
  }

  try {
    const r = await asTenant("app_runtime", A, "SELECT display_name FROM contacts ORDER BY display_name");
    const names = r.rows.map((x) => x.display_name);
    if (names.length === 1 && names[0] === "Alice") ok(`tenant A sees only its own contacts (${JSON.stringify(names)})`);
    else bad("tenant A sees only its own contacts", `got ${JSON.stringify(names)}`);
  } catch (e) {
    bad("tenant A sees only its own contacts", e);
  }

  try {
    const r = await asTenant("app_runtime", B, "SELECT display_name FROM contacts ORDER BY display_name");
    const names = r.rows.map((x) => x.display_name);
    if (names.length === 1 && names[0] === "Bob") ok(`tenant B sees only its own contacts (${JSON.stringify(names)})`);
    else bad("tenant B sees only its own contacts", `got ${JSON.stringify(names)}`);
  } catch (e) {
    bad("tenant B sees only its own contacts", e);
  }

  // The fail-closed case: a pooled connection that was never scoped must see
  // nothing, not everything.
  try {
    const r = await asTenant("app_runtime", null, "SELECT display_name FROM contacts");
    if (r.rows.length === 0) ok("unscoped connection sees zero rows (fail-closed)");
    else bad("unscoped connection sees zero rows (fail-closed)", `leaked ${r.rows.length} rows`);
  } catch (e) {
    bad("unscoped connection sees zero rows (fail-closed)", e);
  }

  // ---------------------------------------------- isolation: write path
  console.log("\n== tenant isolation: write path ==");

  try {
    await asTenant("app_runtime", A,
      `INSERT INTO contacts (tenant_id, display_name) VALUES ($1,'Mallory')`, [B]);
    bad("cross-tenant INSERT is rejected", "insert succeeded");
  } catch (e) {
    if (/row-level security|violates/i.test(e.message)) ok("cross-tenant INSERT is rejected by WITH CHECK");
    else bad("cross-tenant INSERT is rejected", e);
  }

  // Every tenant-prefixed composite FK must reject a globally-valid id that
  // belongs to a different tenant. A plain FK would accept these, because the
  // ids really do exist.
  const crossTenantRefs = [
    {
      name: "contact_identities.contact_id",
      sql: `INSERT INTO contact_identities (tenant_id, contact_id, channel_account_id, phone_e164)
             VALUES ($1,$2,$3,'+15559999999')`,
      params: [A, c2.id, ca.id],
    },
    {
      name: "contact_identities.channel_account_id",
      sql: `INSERT INTO contact_identities (tenant_id, contact_id, channel_account_id, phone_e164)
             VALUES ($1,$2,$3,'+15558888888')`,
      params: [A, c1.id, ca2.id],
    },
    {
      name: "conversations.contact_identity_id",
      sql: `INSERT INTO conversations (tenant_id, channel_account_id, contact_identity_id)
             VALUES ($1,$2,$3)`,
      params: [A, ca.id, i2.id],
    },
    {
      name: "conversations.channel_account_id",
      sql: `INSERT INTO conversations (tenant_id, channel_account_id, contact_identity_id)
             VALUES ($1,$2,$3)`,
      params: [A, ca2.id, i1.id],
    },
    {
      name: "send_intents.contact_identity_id",
      sql: `INSERT INTO send_intents
               (tenant_id, channel_account_id, contact_identity_id, message_type,
                payload_ref, idempotency_key, request_hash)
             VALUES ($1,$2,$3,'text','body://x','key-xt-1','h1')`,
      params: [A, ca.id, i2.id],
    },
    {
      name: "send_intents.channel_account_id",
      sql: `INSERT INTO send_intents
               (tenant_id, channel_account_id, contact_identity_id, message_type,
                payload_ref, idempotency_key, request_hash)
             VALUES ($1,$2,$3,'text','body://x','key-xt-2','h2')`,
      params: [A, ca2.id, i1.id],
    },
  ];

  for (const ref of crossTenantRefs) {
    try {
      await asTenant("app_runtime", A, ref.sql, ref.params);
      bad(`composite FK blocks cross-tenant ${ref.name}`, "insert succeeded");
    } catch (e) {
      if (/row-level security|foreign key|violates/i.test(e.message))
        ok(`composite FK blocks cross-tenant ${ref.name}`);
      else bad(`composite FK blocks cross-tenant ${ref.name}`, e);
    }
  }

  // ------------------------------------------- isolation: update + delete
  console.log("\n== tenant isolation: update and delete ==");
  // Tenant A legitimately owns one contact (Alice), so "affected 1" is the
  // correct outcome. What must not happen is Alice's change reaching Bob, and
  // what must fail is any statement aimed at another tenant's row.
  try {
    const r = await asTenant("app_runtime", A, "UPDATE contacts SET display_name = 'Alice Renamed'");
    if (r.affectedRows === 1) ok("tenant A updates only its own contact (1 row)");
    else bad("tenant A updates only its own contact (1 row)", `affected ${r.affectedRows}`);

    const b = await db.query("SELECT display_name FROM contacts WHERE tenant_id = $1", [B]);
    if (b.rows[0] && b.rows[0].display_name === "Bob")
      ok("tenant A's update did not reach tenant B's contact");
    else bad("tenant A's update did not reach tenant B's contact", JSON.stringify(b.rows[0]));
  } catch (e) {
    bad("tenant A updates only its own contact", e);
  }

  try {
    const r = await asTenant("app_runtime", A, "DELETE FROM contacts WHERE display_name = 'Bob'");
    if (r.affectedRows === 0) ok("cross-tenant DELETE by predicate is a no-op");
    else bad("cross-tenant DELETE by predicate is a no-op", `deleted ${r.affectedRows}`);
  } catch (e) {
    bad("cross-tenant DELETE by predicate is a no-op", e);
  }

  // -------------------------------------- cross-tenant tables are opaque
  console.log("\n== ingress tables are not runtime-readable ==");
  try {
    await asTenant("app_runtime", A, "SELECT * FROM ingress_items");
    bad("app_runtime cannot read ingress_items", "read succeeded");
  } catch (e) {
    if (/permission denied/i.test(e.message)) ok("app_runtime cannot read ingress_items");
    else bad("app_runtime cannot read ingress_items", e);
  }

  try {
    await asTenant("app_runtime", A, "SELECT * FROM tenant_routing_bindings");
    bad("app_runtime cannot read tenant_routing_bindings", "read succeeded");
  } catch (e) {
    if (/permission denied/i.test(e.message)) ok("app_runtime cannot read tenant_routing_bindings");
    else bad("app_runtime cannot read tenant_routing_bindings", e);
  }

  // The ingress role is the one exception, and it still cannot see business data.
  try {
    await asTenant("app_ingress", A, "SELECT display_name FROM contacts");
    bad("app_ingress cannot read tenant business data", "read succeeded");
  } catch (e) {
    if (/permission denied/i.test(e.message)) ok("app_ingress cannot read tenant business data");
    else bad("app_ingress cannot read tenant business data", e);
  }

  // ------------------------------------- app_ingress can ingest, cannot read back
  console.log("\n== app_ingress ingest path ==");
  try {
    await asTenant("app_ingress", A, `
      INSERT INTO ingress_items
        (provider_app_id, item_fingerprint, payload, app_secret_ref,
         signature_verified, phone_number_id)
      VALUES ('app-x', 'fp-ingest-1', '\\x0102'::bytea, 'env:TEST', true, 'pn-1')
    `);
    ok("app_ingress can insert into ingress_items");
  } catch (e) {
    bad("app_ingress can insert into ingress_items", e);
  }

  // Dedupe is index-enforced. app_ingress has no SELECT, so the append path
  // cannot use ON CONFLICT; it inserts plainly and treats 23505 as the
  // duplicate signal, rolling back to a savepoint so the rest of the
  // transaction survives. This asserts that contract actually holds.
  try {
    const outcome = await db.transaction(async (tx) => {
      await tx.exec("SET LOCAL ROLE app_ingress");
      const ins = (fp) => tx.query(`
        INSERT INTO ingress_items
          (provider_app_id, item_fingerprint, payload, app_secret_ref, signature_verified)
        VALUES ('app-x', $1, '\\x0102'::bytea, 'env:TEST', true)
      `, [fp]);

      let stored = 0, duplicates = 0;
      for (const fp of ["fp-dedupe", "fp-dedupe"]) {
        await tx.exec("SAVEPOINT sp");
        try {
          await ins(fp);
          await tx.exec("RELEASE SAVEPOINT sp");
          stored++;
        } catch (e) {
          await tx.exec("ROLLBACK TO SAVEPOINT sp");
          await tx.exec("RELEASE SAVEPOINT sp");
          if (e.code === "23505") duplicates++;
          else throw e;
        }
      }
      // Proves the savepoint rollback left the transaction usable.
      const [probe] = (await tx.query("SELECT 1 AS ok")).rows;
      return { stored, duplicates, alive: probe.ok === 1 };
    });

    if (outcome.stored === 1 && outcome.duplicates === 1 && outcome.alive)
      ok("duplicate detected by unique index, transaction survives");
    else bad("duplicate detected by unique index, transaction survives", JSON.stringify(outcome));
  } catch (e) {
    bad("duplicate detected by unique index, transaction survives", e);
  }

  // Reading raw inbound bodies back is a data leak; the ingest role must not.
  try {
    const r = await asTenant("app_ingress", A, "SELECT payload FROM ingress_items");
    if (r.rows.length === 0) ok("app_ingress cannot read back what it wrote");
    else bad("app_ingress cannot read back what it wrote", `read ${r.rows.length} rows`);
  } catch (e) {
    if (/permission denied/i.test(e.message)) ok("app_ingress cannot read back what it wrote (privilege revoked)");
    else bad("app_ingress cannot read back what it wrote", e);
  }

  // app_ingress may read routing, since that is how it resolves a tenant.
  try {
    const r = await asTenant("app_ingress", A,
      "SELECT tenant_id FROM tenant_routing_bindings WHERE phone_number_id = 'pn-1'");
    if (r.rows.length === 1) ok("app_ingress can read routing bindings");
    else bad("app_ingress can read routing bindings", `got ${r.rows.length} rows`);
  } catch (e) {
    bad("app_ingress can read routing bindings", e);
  }

  // Routing is operator-maintained: the ingest role must not rebind a number.
  try {
    await asTenant("app_ingress", A, `
      INSERT INTO tenant_routing_bindings (provider_app_id, phone_number_id, tenant_id, channel_account_id)
      VALUES ('app-x', 'pn-hijack', $1, $2)
    `, [A, ca.id]);
    bad("app_ingress cannot insert routing bindings", "insert succeeded");
  } catch (e) {
    if (/permission denied|row-level security|violates/i.test(e.message))
      ok("app_ingress cannot insert routing bindings");
    else bad("app_ingress cannot insert routing bindings", e);
  }

  // ------------------------------------------------- ordering + dedupe
  console.log("\n== inbound dedupe (v2 section 39: duplicate webhook) ==");
  try {
    await db.query(`
      INSERT INTO conversations (tenant_id, channel_account_id, contact_identity_id)
      SELECT tenant_id, channel_account_id, contact_identity_id FROM conversations
      WHERE tenant_id = $1
      RETURNING id
    `, [A]);
    bad("second open thread for same identity is rejected", "insert succeeded");
  } catch (e) {
    if (/one_open_per_identity|duplicate key|unique/i.test(e.message))
      ok("second open thread for same identity is rejected");
    else bad("second open thread for same identity is rejected", e);
  }

  try {
    const [conv] = (await db.query(`
      SELECT id FROM conversations WHERE tenant_id = $1
    `, [A])).rows;
    for (const seq of [2, 3]) {
      await db.query(`
        INSERT INTO messages
          (tenant_id, conversation_id, channel_account_id, direction,
           message_type, provider_message_id, aggregate_seq)
        SELECT tenant_id, id, channel_account_id, 'inbound','text',
               'wamid.duplicate', $2
        FROM conversations WHERE id = $1
      `, [conv.id, seq]);
    }
    bad("duplicate provider_message_id is rejected", "both inserts succeeded");
  } catch (e) {
    if (/duplicate key|unique/i.test(e.message)) ok("duplicate provider_message_id is rejected");
    else bad("duplicate provider_message_id is rejected", e);
  }

  // ------------------------------------------- status ordering guarantee
  console.log("\n== out-of-order status (v2 section 39) ==");
  try {
    const [msg] = (await db.query(`
      SELECT id, tenant_id FROM messages WHERE provider_message_id = 'wamid.duplicate'
    `)).rows;

    // 'read' arrives first, then a late 'sent'.
    for (const status of ["read", "sent"]) {
      await db.query(`
        INSERT INTO status_observations
          (tenant_id, channel_account_id, provider_message_id, message_id,
           status, dedupe_key)
        VALUES ($1,$2,'wamid.duplicate',$3,$4,$5)
      `, [msg.tenant_id, ca.id, msg.id, status, `wamid.duplicate:${status}`]);
    }

    const [proj] = (await db.query(`
      SELECT latest_status, rank FROM message_delivery_projection
      WHERE tenant_id = $1 AND message_id = $2
    `, [msg.tenant_id, msg.id])).rows;

    if (proj && proj.latest_status === "read" && proj.rank === 4)
      ok(`late 'sent' does not regress 'read' projection (status=${proj.latest_status})`);
    else bad("late 'sent' does not regress 'read' projection", `got ${JSON.stringify(proj)}`);
  } catch (e) {
    bad("late 'sent' does not regress 'read' projection", e);
  }

  // -------------------------------------------- orphan status observation
  console.log("\n== orphan status observation (receipt races send response) ==");
  try {
    const r = await db.query(`
      INSERT INTO status_observations
        (tenant_id, channel_account_id, provider_message_id, status, dedupe_key)
      VALUES ($1,$2,'wamid.not-yet-mapped','delivered','wamid.not-yet-mapped:delivered')
      RETURNING id, send_intent_id
    `, [A, ca.id]);

    if (r.rows[0] && r.rows[0].send_intent_id === null)
      ok("status for an unmapped message is stored as an orphan, not dropped");
    else bad("status for an unmapped message is stored as an orphan", JSON.stringify(r.rows[0]));
  } catch (e) {
    bad("status for an unmapped message is stored as an orphan", e);
  }

  // ------------------------------------------- quota reservation unique
  console.log("\n== quota reservation is unique per intent/meter/recipient ==");
  try {
    const [intent] = (await db.query(`
      INSERT INTO send_intents
        (tenant_id, channel_account_id, contact_identity_id, message_type,
         payload_ref, idempotency_key, request_hash)
      VALUES ($1,$2,$3,'text','body://x','key-1','hash-1')
      RETURNING id
    `, [A, ca.id, i1.id])).rows;

    const ins = () => db.query(`
      INSERT INTO quota_reservations
        (tenant_id, send_intent_id, scope_kind, scope_id, meter, recipient_key, units)
      VALUES ($1,$2,'phone_number','pn-1','unique_recipients_24h','+15550000001',1)
    `, [A, intent.id]);

    await ins();
    try {
      await ins();
      bad("second reservation for same recipient is rejected", "both inserts succeeded");
    } catch (e) {
      if (/duplicate key|unique/i.test(e.message)) ok("second reservation for same recipient is rejected");
      else bad("second reservation for same recipient is rejected", e);
    }
  } catch (e) {
    bad("quota reservation uniqueness", e);
  }

  // ------------------------------------------- idempotency key conflict
  console.log("\n== idempotency key is unique per tenant+operation ==");
  try {
    await db.query(`
      INSERT INTO send_intents
        (tenant_id, channel_account_id, contact_identity_id, message_type,
         payload_ref, idempotency_key, request_hash)
      VALUES ($1,$2,$3,'text','body://y','key-1','hash-2')
    `, [A, ca.id, i1.id]);
    bad("replayed idempotency key is rejected", "insert succeeded");
  } catch (e) {
    if (/duplicate key|unique/i.test(e.message)) ok("replayed idempotency key is rejected");
    else bad("replayed idempotency key is rejected", e);
  }

  // ------------------------------------------------- isolation assertion
  console.log("\n== privilege assertions ==");
  try {
    await db.exec("SELECT app_assert_isolation_principles()");
    ok("no runtime role holds BYPASSRLS or SUPERUSER");
  } catch (e) {
    bad("no runtime role holds BYPASSRLS or SUPERUSER", e);
  }

  // ----------------------------------------------------------- summary
  console.log(`\n${"=".repeat(52)}`);
  console.log(`  passed ${passed}   failed ${failed}`);
  console.log("=".repeat(52));
  console.log("\nnot covered here (needs a real server):");
  console.log("  - FOR UPDATE SKIP LOCKED under two concurrent workers");
  console.log("  - job lease expiry and reclaim races");
  console.log("  - transactional outbox relay throughput");
  console.log("  - connection pooling behaviour with app.tenant_id reset");

  process.exit(failed === 0 ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
