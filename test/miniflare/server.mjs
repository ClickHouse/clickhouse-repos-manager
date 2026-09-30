// Local R2 with the S3 API at http://<host>:<port>/cdn-cgi/local/r2/s3/<bucket>.
// State is in memory: every process starts with an empty bucket.
import { Miniflare } from "miniflare";

const bucket = process.env.MF_BUCKET ?? "packages-dev";
const credentials = {
  accessKeyId: process.env.MF_ACCESS_KEY_ID ?? "A".repeat(32),
  secretAccessKey: process.env.MF_SECRET_ACCESS_KEY ?? "test-secret-key",
};
const mf = new Miniflare({
  host: process.env.MF_HOST ?? "127.0.0.1",
  port: Number(process.env.MF_PORT ?? 8787),
  workers: [
    {
      config: {
        name: "",
        compatibilityDate: "2025-05-01",
        manifest: {
          mainModule: "index.mjs",
          modulesRoot: process.cwd(),
          modules: {
            "index.mjs": {
              type: "esm",
              contents: "export default { fetch() { return new Response(null, { status: 404 }) } }",
            },
          },
        },
        env: {
          BUCKET: { type: "r2", name: bucket, dev: { experimentalS3Credentials: credentials } },
        },
      },
    },
  ],
});
const url = await mf.ready;
console.log(`ready ${new URL("/cdn-cgi/local/r2/s3", url).href}`);
for (const sig of ["SIGTERM", "SIGINT"]) {
  process.on(sig, async () => {
    await mf.dispose();
    process.exit(0);
  });
}
