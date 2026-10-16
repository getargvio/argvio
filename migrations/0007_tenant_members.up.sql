-- Maps an identity at an OIDC provider to the tenants it may use. Identities
-- are never read off a token claim: the server resolves a verified
-- (issuer, subject) pair here, so a user's access is revocable and a user can
-- belong to several tenants.
CREATE TABLE tenant_members (
    tenant_id  UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- Compared byte-for-byte with the token's `iss`, so it must be the exact
    -- string the provider's discovery document states (trailing slash and all).
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    role       TEXT NOT NULL DEFAULT 'viewer',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, issuer, subject)
);

-- Per-request lookup is by identity, the reverse of the primary key's order.
CREATE INDEX idx_tenant_members_identity ON tenant_members (issuer, subject);
