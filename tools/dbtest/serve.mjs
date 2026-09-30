// Serves the migrated schema over the Postgres wire protocol on TCP, so the Go
// integration tests can exercise the real appender with real SQL, real roles
// and real RLS instead of an in-memory double.
//
// PGlite serialises every query behind one global queue, so this proves
// correctness, not concurrency. Concurrency still needs a real Postgres.

// pglite-socket dispatches DOM CustomEvent objects, which only became a Node
// global in Node 19. This project runs on Node 18, so the minimal shape the
// library uses is polyfilled before anything connects. Without it the server
// throws on its first accepted connection.
if (typeof globalThis.CustomEvent === "undefined") {
  globalThis.CustomEvent = class CustomEvent extends Event {
    constructor(type, init = {}) {
      super(type);
      this.detail = init.detail;
    }
  };
}

import { PGlite } from "@electric-sql/pglite";
import { PGLiteSocketServer } from "@electric-sql/pglite-socket";
import { readdir, readFile, rm } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const migrationsDir = join(here, "..", "..", "db", "migrations");

export const SEED = {
  tenantId: "11111111-1111-1111-1111-111111111111",
  tenantSlug: "acme",
  otherTenantId: "22222222-2222-2222-2222-222222222222",
  providerAppId: "dev-app-1",
  phoneNumberId: "pn-dev-1",
  otherPhoneNumberId: "pn-dev-2",
  wabaId: "waba-dev-1",
  credentialRef: "env:META_APP_SECRET",
};

export async function migrate(db) {
  const files = (await readdir(migrationsDir)).filter((f) => f.endsWith(".sql")).sort();
  for (const file of files) {
    const sql = await readFile(join(migrationsDir, file), "utf8");
    await db.transaction(async (tx) => {
      await tx.exec(sql);
    });
  }
  return files.length;
}

// Fixtures mirror what a real deployment would have configured: one tenant
// owning two numbers, plus the routing bindings that let ingress resolve a
// phone number to a tenant before any conversation exists.
export async function seed(db) {
  const s = SEED;
  await db.exec(`
    INSERT INTO tenants (id, slug, name) VALUES
      ('${s.tenantId}', '${s.tenantSlug}', 'Acme'),
      ('${s.otherTenantId}', 'globex', 'Globex')
    ON CONFLICT DO NOTHING;

    INSERT INTO channel_accounts
      (tenant_id, provider_app_id, waba_id, phone_number_id, status, credential_ref)
    VALUES
      ('${s.tenantId}', '${s.providerAppId}', '${s.wabaId}', '${s.phoneNumberId}',
       'active', '${s.credentialRef}'),
      ('${s.otherTenantId}', '${s.providerAppId}', '${s.wabaId}', '${s.otherPhoneNumberId}',
       'active', '${s.credentialRef}')
    ON CONFLICT DO NOTHING;

    INSERT INTO tenant_routing_bindings
      (provider_app_id, phone_number_id, tenant_id, channel_account_id)
    SELECT '${s.providerAppId}', '${s.phoneNumberId}', ca.tenant_id, ca.id
      FROM channel_accounts ca WHERE ca.phone_number_id = '${s.phoneNumberId}'
    ON CONFLICT DO NOTHING;

    INSERT INTO tenant_routing_bindings
      (provider_app_id, phone_number_id, tenant_id, channel_account_id)
    SELECT '${s.providerAppId}', '${s.otherPhoneNumberId}', ca.tenant_id, ca.id
      FROM channel_accounts ca WHERE ca.phone_number_id = '${s.otherPhoneNumberId}'
    ON CONFLICT DO NOTHING;
  `);
}

// Where the database lives on disk. Persisting it matters for the end-to-end
// check: an in-memory database exists only inside this process, so another
// process could not independently read back what the service wrote.
export const DATA_DIR = process.env.PGLITE_DATA_DIR ?? join(here, ".pgdata");

async function main() {
  const port = Number(process.env.PGLITE_PORT ?? 55432);
  const host = process.env.PGLITE_HOST ?? "127.0.0.1";

  const fresh = process.env.PGLITE_FRESH === "1";
  if (fresh) await rm(DATA_DIR, { recursive: true, force: true });

  const db = await PGlite.create(DATA_DIR);

  // Migrations are written to be idempotent at the object level, but applying
  // them twice to the same data directory is not a scenario worth supporting.
  const count = await migrate(db);
  await seed(db);
  console.log(`applied ${count} migrations and seeded fixtures in ${DATA_DIR}`);

  const server = new PGLiteSocketServer({ db, port, host, maxConnections: 8 });
  await server.start();
  console.log(`listening on ${host}:${port}`);

  const shutdown = async () => {
    await server.stop();
    await db.close();
    process.exit(0);
  };
  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((e) => {
    console.error(e);
    process.exit(1);
  });
}