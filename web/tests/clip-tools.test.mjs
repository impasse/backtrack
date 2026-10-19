import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
	adjustClipBoundary,
	clipMediaBounds,
	mediaToWall,
	parseRecordingPlaylist,
	recentClipWindow,
} from '../src/lib/clip-tools.ts';

const base = Date.parse('2026-10-19T03:52:20+08:00');
const spans = [
	{ mediaStart: 0, mediaEnd: 5, wallStart: base },
	{ mediaStart: 5, mediaEnd: 10, wallStart: base + 14_000 },
];

test('playlist mapping preserves a disconnect gap and continues dates within a fragment sequence', () => {
	const parsed = parseRecordingPlaylist(
		`#EXTM3U\n#EXT-X-PROGRAM-DATE-TIME:${new Date(base).toISOString()}\n#EXTINF:5,\n/a.m4s\n#EXT-X-DISCONTINUITY\n#EXT-X-PROGRAM-DATE-TIME:${new Date(base + 14_000).toISOString()}\n#EXTINF:3,\n/b.m4s\n#EXTINF:2,\n/c.m4s`,
	);
	assert.deepEqual(parsed, [
		spans[0],
		{ mediaStart: 5, mediaEnd: 8, wallStart: base + 14_000 },
		{ mediaStart: 8, mediaEnd: 10, wallStart: base + 17_000 },
	]);
	assert.equal(mediaToWall(parsed, 5), base + 14_000);
	assert.equal(mediaToWall(parsed, 10), base + 19_000);
});

test('playlist parsing skips invalid durations and does not invent an unknown recording date', () => {
	assert.deepEqual(parseRecordingPlaylist('#EXTM3U\n#EXTINF:2,\n/a.m4s\n#EXTINF:NaN,\n/b.m4s'), []);
});

test('preview bounds skip missing wall time and retain subsecond cut points', () => {
	assert.deepEqual(clipMediaBounds(spans, base + 4200, base + 15_100), { start: 4.2, end: 6.1 });
	assert.deepEqual(clipMediaBounds(spans, base + 8000, base + 15_000), { start: 5, end: 6 });
	assert.deepEqual(clipMediaBounds(spans, base + 2000, base + 8000), { start: 2, end: 5 });
	assert.equal(clipMediaBounds(spans, base + 6000, base + 13_000), undefined);
});

test('media times outside the loaded map have no guessed wall time', () => {
	assert.equal(mediaToWall(spans, -1), undefined);
	assert.equal(mediaToWall(spans, 11), undefined);
});

test('recent selection uses the visible playhead rather than the newest recording', () => {
	const continuous = [{ mediaStart: 0, mediaEnd: 180, wallStart: base }];
	assert.deepEqual(recentClipWindow(continuous, 30, base + 90_000), {
		start: base + 60_000,
		end: base + 90_000,
		truncated: false,
		hasGaps: false,
	});
	assert.equal(recentClipWindow(continuous, 30, base - 1000), undefined);
});

test('without a playhead recent selection ends at the latest committed media and clamps short recordings', () => {
	assert.deepEqual(recentClipWindow(spans, 30), {
		start: base,
		end: base + 19_000,
		truncated: true,
		hasGaps: true,
	});
	assert.deepEqual(recentClipWindow(spans, 3, base + 999_000), {
		start: base + 16_000,
		end: base + 19_000,
		truncated: false,
		hasGaps: false,
	});
});

test('recent selection inside a disconnect gap ends at the preceding recorded frame', () => {
	assert.deepEqual(recentClipWindow(spans, 3, base + 9000), {
		start: base + 2000,
		end: base + 5000,
		truncated: false,
		hasGaps: false,
	});
});

test('invalid or unavailable recent selections cannot create a clip window', () => {
	assert.equal(recentClipWindow([], 30), undefined);
	assert.equal(recentClipWindow(spans, 0), undefined);
	assert.equal(recentClipWindow(spans, 30, NaN), undefined);
});

test('nudge and drag clamp within the recording and keep a 0.1 second minimum selection', () => {
	assert.equal(adjustClipBoundary('start', -100, 1000, 2000, 0, 5000), 0);
	assert.equal(adjustClipBoundary('start', 2500, 1000, 2000, 0, 5000), 1900);
	assert.equal(adjustClipBoundary('end', 500, 1000, 2000, 0, 5000), 1100);
	assert.equal(adjustClipBoundary('end', 6000, 1000, 2000, 0, 5000), 5000);
	assert.equal(adjustClipBoundary('start', NaN, 1000, 2000, 0, 5000), undefined);
});
