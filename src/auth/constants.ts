/**
 * OAuth 2.0 constants shared with the vault-share Google Cloud project and relay worker.
 *
 * @packageDocumentation
 */

/** Plugin ID of the legacy V1 sister plugin sharing the same OAuth protocol handler. */
export const VAULT_SHARE_PLUGIN_ID = 'vault-share';

/** Protocol action registered with `registerObsidianProtocolHandler` for OAuth callbacks. */
export const OAUTH_PROTOCOL_ACTION = 'vault-share-auth';

/** Google OAuth 2.0 client ID shared with vault-share. */
export const GOOGLE_CLIENT_ID = '535881224719-5qvscbujq78mpvne1rp3im0cg5cdcu4c.apps.googleusercontent.com';

/** Cloudflare Worker URL relaying Google's authorization callback and token refresh. */
export const RELAY_BASE_URL = 'https://vault-share-auth.bob-hyman.workers.dev';
