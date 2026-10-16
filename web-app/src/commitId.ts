/**
 * Identity of one document update submission. Retried submits of the same
 * unconfirmed change reuse the id so the server never writes paperless-ngx
 * twice; a fresh user action (or a definitive server rejection) starts a new
 * one. Kept outside component modules so react-refresh stays happy.
 */
export function newCommitId(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `commit-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}
