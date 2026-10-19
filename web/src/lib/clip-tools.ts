export type PlaybackTimeSpan = { mediaStart: number; mediaEnd: number; wallStart: number };

export function parseRecordingPlaylist(playlist: string): PlaybackTimeSpan[] {
	const spans: PlaybackTimeSpan[] = [];
	let wallStart: number | undefined;
	let mediaStart = 0;
	for (const line of playlist.split(/\r?\n/)) {
		if (line.startsWith('#EXT-X-PROGRAM-DATE-TIME:')) {
			const value = Date.parse(line.slice('#EXT-X-PROGRAM-DATE-TIME:'.length));
			wallStart = Number.isFinite(value) ? value : undefined;
		} else if (line.startsWith('#EXTINF:')) {
			const duration = Number.parseFloat(line.slice('#EXTINF:'.length));
			if (!Number.isFinite(duration) || duration <= 0) continue;
			if (wallStart !== undefined) {
				spans.push({ mediaStart, mediaEnd: mediaStart + duration, wallStart });
				wallStart += duration * 1000;
			}
			mediaStart += duration;
		}
	}
	return spans;
}

export function mediaToWall(spans: PlaybackTimeSpan[], position: number): number | undefined {
	const span =
		spans.find((item) => position >= item.mediaStart && position < item.mediaEnd) ??
		(position === spans.at(-1)?.mediaEnd ? spans.at(-1) : undefined);
	return span ? span.wallStart + (position - span.mediaStart) * 1000 : undefined;
}

export function clipMediaBounds(spans: PlaybackTimeSpan[], start: number, end: number) {
	if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return undefined;
	let mediaStart: number | undefined;
	let mediaEnd: number | undefined;
	for (const span of spans) {
		const wallEnd = span.wallStart + (span.mediaEnd - span.mediaStart) * 1000;
		const a = Math.max(start, span.wallStart);
		const b = Math.min(end, wallEnd);
		if (b <= a) continue;
		mediaStart ??= span.mediaStart + (a - span.wallStart) / 1000;
		mediaEnd = span.mediaStart + (b - span.wallStart) / 1000;
	}
	return mediaStart !== undefined && mediaEnd !== undefined && mediaEnd > mediaStart
		? { start: mediaStart, end: mediaEnd }
		: undefined;
}

export function recentClipWindow(spans: PlaybackTimeSpan[], seconds: number, playhead?: number) {
	if (!spans.length || !Number.isFinite(seconds) || seconds <= 0) return undefined;
	const first = spans[0];
	const last = spans.at(-1)!;
	const lastEnd = last.wallStart + (last.mediaEnd - last.mediaStart) * 1000;
	const requestedEnd = playhead === undefined ? lastEnd : Math.min(playhead, lastEnd);
	if (!Number.isFinite(requestedEnd) || requestedEnd <= first.wallStart) return undefined;
	// End at recorded media when the playhead falls inside a disconnect gap.
	const lastIncluded = spans.findLast((span) => span.wallStart < requestedEnd);
	if (!lastIncluded) return undefined;
	const end = Math.min(
		requestedEnd,
		lastIncluded.wallStart + (lastIncluded.mediaEnd - lastIncluded.mediaStart) * 1000,
	);
	const requestedStart = end - seconds * 1000;
	const firstIncluded = spans.find(
		(span) => span.wallStart + (span.mediaEnd - span.mediaStart) * 1000 > requestedStart,
	);
	if (!firstIncluded) return undefined;
	const start = Math.max(requestedStart, firstIncluded.wallStart);
	const bounds = clipMediaBounds(spans, start, end);
	if (!bounds) return undefined;
	return {
		start,
		end,
		truncated: start > requestedStart,
		hasGaps: (end - start) / 1000 - (bounds.end - bounds.start) > 0.05,
	};
}

export function adjustClipBoundary(
	boundary: 'start' | 'end',
	value: number,
	start: number,
	end: number,
	rangeStart: number,
	rangeEnd: number,
) {
	const min = boundary === 'start' ? rangeStart : start + 100;
	const max = boundary === 'start' ? end - 100 : rangeEnd;
	return Number.isFinite(value) && max >= min
		? Math.round(Math.max(min, Math.min(max, value)))
		: undefined;
}
