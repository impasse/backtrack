import { api } from '$lib/api';

export const authChangeEvent = 'dvr-auth-changed';
export type AuthState = { authenticated: boolean; role?: 'admin' | 'viewer' | '' };

export function notifyAuthChange(state: AuthState) {
	window.dispatchEvent(new CustomEvent(authChangeEvent, { detail: state }));
}

export function connectDvrEvents(
	listeners: Record<string, EventListener>,
	onAuthChange?: (state: AuthState) => void,
): () => void {
	let source: EventSource | undefined;
	let active = true;
	let generation = 0;
	const sync = async () => {
		const currentGeneration = ++generation;
		source?.close();
		source = undefined;
		let state: AuthState = { authenticated: false, role: '' };
		try {
			state = await api<AuthState>('/api/auth/session');
		} catch {
			state = { authenticated: false, role: '' };
		}
		if (!active || currentGeneration !== generation) return;
		onAuthChange?.(state);
		if (!state.authenticated) return;
		source = new EventSource('/api/events');
		for (const [type, listener] of Object.entries(listeners))
			source.addEventListener(type, listener);
	};
	window.addEventListener(authChangeEvent, sync);
	void sync();
	return () => {
		active = false;
		generation++;
		window.removeEventListener(authChangeEvent, sync);
		source?.close();
	};
}
