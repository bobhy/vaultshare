import { describe, expect, it, vi } from 'vitest';
import type { App } from 'obsidian';
import {
	LEGACY_VAULT_SHARE_PLUGIN_ID,
	OAuthFlow,
	isLegacyVaultShareEnabled,
	refreshTokenSecretKey,
} from './oauth';

function makeMockApp(options: {
	vaultName?: string;
	enabledPlugins?: string[];
	omitRegistry?: boolean;
	communityPluginsJson?: string;
}): App {
	return {
		vault: {
			configDir: '.obsidian',
			getName: () => options.vaultName ?? 'My Vault',
			adapter: {
				exists: vi.fn((p: string) => Promise.resolve(p === '.obsidian/community-plugins.json' && options.communityPluginsJson !== undefined)),
				read: vi.fn(() => Promise.resolve(options.communityPluginsJson ?? '[]')),
			},
		},
		...(options.omitRegistry
			? {}
			: {
				plugins: {
					enabledPlugins: new Set(options.enabledPlugins ?? []),
				},
			}),
	} as unknown as App;
}

describe('isLegacyVaultShareEnabled', () => {
	it('returns false when vault-share is absent or installed-but-disabled', async () => {
		const app = makeMockApp({ enabledPlugins: ['other-plugin'] });
		expect(await isLegacyVaultShareEnabled(app)).toBe(false);
	});

	it('returns true when vault-share is active in enabledPlugins', async () => {
		const app = makeMockApp({ enabledPlugins: [LEGACY_VAULT_SHARE_PLUGIN_ID] });
		expect(await isLegacyVaultShareEnabled(app)).toBe(true);
	});

	it('falls back to community-plugins.json when internal registry is absent', async () => {
		const disabledApp = makeMockApp({ omitRegistry: true, communityPluginsJson: '["obsidian-git"]' });
		expect(await isLegacyVaultShareEnabled(disabledApp)).toBe(false);

		const enabledApp = makeMockApp({ omitRegistry: true, communityPluginsJson: '["vault-share"]' });
		expect(await isLegacyVaultShareEnabled(enabledApp)).toBe(true);
	});
});

describe('refreshTokenSecretKey', () => {
	it('sanitizes vault name into a vaultshare-prefixed key', () => {
		expect(refreshTokenSecretKey('Test Vault 1!')).toBe('vaultshare-test-vault-1-refresh-token');
		expect(refreshTokenSecretKey('   ')).toBe('vaultshare-default-refresh-token');
	});
});

describe('OAuthFlow', () => {
	it('verifies one-time CSRF state and extracts refresh token', () => {
		const flow = new OAuthFlow();
		expect(() => flow.handleAuthCallback({ state: 'any', refresh_token: 'rt' })).toThrow('No pending OAuth login flow');

		const authUrl = flow.getAuthorizationUrl();
		const parsed = new URL(authUrl);
		expect(parsed.searchParams.get('scope')).toBe('https://www.googleapis.com/auth/drive.file');
		const state = parsed.searchParams.get('state') ?? '';
		expect(state).not.toBe('');

		expect(() => flow.handleAuthCallback({ state: 'wrong-state', refresh_token: 'rt' })).toThrow('state mismatch');

		const url2 = flow.getAuthorizationUrl();
		const state2 = new URL(url2).searchParams.get('state') ?? '';
		expect(flow.handleAuthCallback({ state: state2, refresh_token: '  refresh-abc  ' })).toBe('refresh-abc');

		// State is one-shot; replaying the same callback fails.
		expect(() => flow.handleAuthCallback({ state: state2, refresh_token: 'refresh-abc' })).toThrow('No pending OAuth login flow');
	});

	it('rejects callbacks missing refresh_token', () => {
		const flow = new OAuthFlow();
		const state = new URL(flow.getAuthorizationUrl()).searchParams.get('state') ?? '';
		expect(() => flow.handleAuthCallback({ state })).toThrow('Refresh token missing');
	});
});
