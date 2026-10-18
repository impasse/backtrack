import { redirect } from '@sveltejs/kit';

export function load({ url }: { url: URL }) {
	const streamId = url.searchParams.get('streamId');
	if (!streamId) redirect(307, '/channels');
	redirect(
		307,
		`/live?streamId=${encodeURIComponent(streamId)}&tab=history`,
	);
}
