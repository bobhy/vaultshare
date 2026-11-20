/**
 * Google OAuth 2.0 browser-consent flow and V1 mutual-exclusion check.
 *
 * VaultShare V2 reuses `vault-share`'s registered OAuth client ID and Cloudflare
 * relay (`RELAY_BASE_URL`) so V1-created `drive.file` files remain accessible
 * without deploying a new cloud project. Only the system-browser authorization
 * URL generation and `obsidian://vault-share-auth` callback verification live in
 * TypeScript; refresh-token persistence (`SecretStorage`) and token refreshing
 * live in Go.
 *
 * @packageDocumentation
 */

import { type App, normalizePath } from 'obsidian';
import {
	GOOGLE_CLIENT_ID,
	LEGACY_VAULT_SHARE_PLUGIN_ID,
	OAUTH_PROTOCOL_ACTION,
	RELAY_BASE_URL,
} from './constants';

export { GOOGLE_CLIENT_ID, LEGACY_VAULT_SHARE_PLUGIN_ID, OAUTH_PROTOCOL_ACTION, RELAY_BASE_URL };

const GOOGLE_AUTH_URL = 'https://accounts.google.com/o/oauth2/v2/auth';
const DRIVE_FILE_SCOPE = 'https://www.googleapis.com/auth/drive.file';
const NONCE_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';

interface ObsidianPluginsRegistry {
	plugins?: {
		enabledPlugins?: Set<string>;
	};
}

/**
 * Returns `true` when the legacy `vault-share` plugin is currently enabled in
 * this vault. Installed-but-disabled `vault-share` does not conflict because it
 * is not active and does not register the shared `vault-share-auth` handler.
 */
export async function isLegacyVaultShareEnabled(app: App): Promise<boolean> {
	const registry = (app as unknown as ObsidianPluginsRegistry).plugins;
	if (registry?.enabledPlugins) {
		return registry.enabledPlugins.has(LEGACY_VAULT_SHARE_PLUGIN_ID);
	}

	const communityPluginsPath = normalizePath(`${app.vault.configDir}/community-plugins.json`);
	try {
		if (!(await app.vault.adapter.exists(communityPluginsPath))) {
			return false;
		}
		const raw = await app.vault.adapter.read(communityPluginsPath);
		const parsed: unknown = JSON.parse(raw);
		return Array.isArray(parsed) && parsed.includes(LEGACY_VAULT_SHARE_PLUGIN_ID);
	} catch {
		return false;
	}
}

/**
 * Computes the vault-scoped `SecretStorage` key used by VaultShare V2 so its
 * refresh token stays isolated from legacy `vault-share` and other vaults.
 */
export function refreshTokenSecretKey(vaultName: string): string {
	const safe = vaultName.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') || 'default';
	return `vaultshare-${safe}-refresh-token`;
}

/**
 * Coordinates the interactive OAuth 2.0 browser login flow and CSRF `state`
 * validation for `obsidian://vault-share-auth` callbacks.
 */
export class OAuthFlow {
	private pendingState: string | null = null;

	/**
	 * Generates a Google OAuth 2.0 authorization URL and stores a one-time CSRF
	 * `state` token for verification when the callback fires.
	 */
	getAuthorizationUrl(): string {
		const nonce = generateNonce(32);
		this.pendingState = btoa(JSON.stringify({ app: 'vault-share', nonce }));

		const params = new URLSearchParams({
			client_id: GOOGLE_CLIENT_ID,
			redirect_uri: `${RELAY_BASE_URL}/google/callback`,
			response_type: 'code',
			scope: DRIVE_FILE_SCOPE,
			access_type: 'offline',
			prompt: 'consent',
			state: this.pendingState,
		});
		return `${GOOGLE_AUTH_URL}?${params.toString()}`;
	}

	/** Opens the generated Google OAuth URL in the user's system browser. */
	startLogin(): void {
		const url = this.getAuthorizationUrl();
		window.open(url, '_blank', 'noopener,noreferrer');
	}

	/**
	 * Verifies an `obsidian://vault-share-auth` callback's CSRF `state` and
	 * returns the Google OAuth refresh token for Go to persist.
	 */
	handleAuthCallback(params: Record<string, string | undefined>): string {
		if (!this.pendingState) {
			throw new Error('No pending OAuth login flow. Start from plugin settings.');
		}
		if (!params.state || params.state !== this.pendingState) {
			throw new Error('OAuth state mismatch. Please try logging in again.');
		}
		this.pendingState = null;

		const token = params.refresh_token?.trim() ?? '';
		if (!token) {
			throw new Error('Refresh token missing from OAuth callback.');
		}
		return token;
	}
}

function generateNonce(length: number): string {
	const limit = 256 - (256 % NONCE_CHARS.length);
	const out: string[] = [];
	while (out.length < length) {
		const bytes = new Uint8Array(length - out.length);
		crypto.getRandomValues(bytes);
		for (const b of bytes) {
			if (b < limit && out.length < length) {
				out.push(NONCE_CHARS.charAt(b % NONCE_CHARS.length));
			}
		}
	}
	return out.join('');
}
