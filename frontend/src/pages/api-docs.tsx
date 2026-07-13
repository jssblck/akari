import SwaggerUI from "swagger-ui-react";

import { PublicShell } from "../components/public-shell";

export function ApiDocsPage() {
  return (
    <PublicShell>
      <div className="api-docs">
        <div className="docs-intro">
          <span className="label">OpenAPI 3.1</span>
          <h1>akari API</h1>
          <p>
            The contract for browser reads, account operations, and session
            ingest. Download the raw document at{" "}
            <a href="/api/openapi.json">/api/openapi.json</a>.
          </p>
        </div>
        <SwaggerUI
          url="/api/openapi.json"
          deepLinking
          displayRequestDuration
          tryItOutEnabled
        />
      </div>
    </PublicShell>
  );
}
