import { userFacingError } from './messages';

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
	const headers = new Headers(init.headers);
	if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
	const res = await fetch(path, { ...init, headers, credentials: 'same-origin' });
	if (!res.ok) {
		const body = await res.json().catch(() => ({}));
		throw new Error(userFacingError(body.error ?? `Request failed (${res.status})`));
	}
	return res.json() as Promise<T>;
}
