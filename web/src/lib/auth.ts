export type AuthRole = 'admin' | 'viewer';
export type AuthSession = { authenticated: boolean; role?: AuthRole | '' };

const tokenStorageKey = 'backtrack.accessToken';

async function requestSession(token?: string): Promise<Response> {
	return fetch('/api/auth/session', {
		method: token ? 'POST' : 'GET',
		credentials: 'same-origin',
		headers: token ? { 'Content-Type': 'application/json' } : undefined,
		body: token ? JSON.stringify({ token }) : undefined,
	});
}

export async function restoreAuthSession(): Promise<AuthSession> {
	try {
		const response = await requestSession();
		if (response.ok) {
			const session = (await response.json()) as AuthSession;
			if (session.authenticated) return session;
		}
	} catch {
		// A saved token may still restore the session if the cookie has expired.
	}

	const token = localStorage.getItem(tokenStorageKey);
	if (!token) return { authenticated: false, role: '' };

	try {
		const response = await requestSession(token);
		if (response.ok) return (await response.json()) as AuthSession;
		if (response.status === 401) localStorage.removeItem(tokenStorageKey);
	} catch {
		// Keep the token during an outage so it can be retried next time.
	}
	return { authenticated: false, role: '' };
}

export async function loginWithToken(token: string, remember = true): Promise<AuthSession> {
	const cleanToken = token.trim();
	if (!cleanToken) throw new Error('请输入访问令牌');

	const response = await requestSession(cleanToken);
	const body = (await response.json().catch(() => ({}))) as AuthSession & { error?: string };
	if (!response.ok)
		throw new Error(
			body.error === 'invalid access token'
				? '令牌无效，请检查后重试。'
				: (body.error ?? `登录失败（${response.status}）`),
		);

	if (remember) localStorage.setItem(tokenStorageKey, cleanToken);
	else localStorage.removeItem(tokenStorageKey);
	return body;
}

export async function logoutSession(): Promise<void> {
	localStorage.removeItem(tokenStorageKey);
	await fetch('/api/auth/session', { method: 'DELETE', credentials: 'same-origin' }).catch(
		() => {},
	);
}
