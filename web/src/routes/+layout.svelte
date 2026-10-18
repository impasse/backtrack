<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { logoutSession, restoreAuthSession, type AuthRole, type AuthSession } from '$lib/auth';
	import { notifyAuthChange } from '$lib/events';
	let { children } = $props();
	let authReady = $state(false);
	let authenticated = $state(false);
	let savedRole = $state<AuthRole | ''>('');
	function nextPath() {
		const next = page.url.searchParams.get('next');
		return next?.startsWith('/') && !next.startsWith('//') && !next.startsWith('/login')
			? next
			: '/channels';
	}
	onMount(() => {
		let active = true;
		const onAuthChange = (event: Event) => {
			const state = (event as CustomEvent<AuthSession>).detail;
			authenticated = state.authenticated;
			savedRole = state.role ?? '';
		};
		window.addEventListener('dvr-auth-changed', onAuthChange);
		void restoreAuthSession().then((session) => {
			if (!active) return;
			authenticated = session.authenticated;
			savedRole = session.role ?? '';
			authReady = true;
			notifyAuthChange(session);
		});
		return () => {
			active = false;
			window.removeEventListener('dvr-auth-changed', onAuthChange);
		};
	});
	$effect(() => {
		if (!authReady) return;
		if (!authenticated && page.url.pathname !== '/login') {
			void goto(`/login?next=${encodeURIComponent(page.url.pathname + page.url.search)}`);
		} else if (authenticated && page.url.pathname === '/login') {
			void goto(nextPath());
		}
	});
	async function clearKey() {
		await logoutSession();
		authenticated = false;
		savedRole = '';
		notifyAuthChange({ authenticated: false, role: '' });
		await goto('/login');
	}
	const links = [
		{ href: '/channels', label: '频道列表', icon: '◉' },
		{ href: '/favorites', label: '收藏片段', icon: '★' },
		{ href: '/settings', label: '设置', icon: '⚙' },
	];
</script>

<svelte:head>
	<title>{page.url.pathname === '/login' ? '登录 · Backtrack DVR' : 'Backtrack DVR'}</title>
	<meta name="description" content="Backtrack 私人直播录像与时移回看服务" />
</svelte:head>
{#if !authReady}
	<div class="auth-loading">
		<span class="loading-mark">B</span>
		<span>正在检查登录状态…</span>
	</div>
{:else if page.url.pathname === '/login'}
	{@render children()}
{:else if authenticated}
	<div class="app-shell">
		<aside class="sidebar">
			<a class="brand" href="/channels">
				<span class="brand-mark">B</span>
				<span>
					Backtrack <small>私人直播档案</small>
				</span>
			</a>
			<div class="nav-label">工作区</div>
			<nav>
				{#each links as item}<a
						class:active={page.url.pathname.startsWith(item.href) ||
							(item.href === '/channels' && page.url.pathname === '/live')}
						href={item.href}
					>
						<span>{item.icon}</span>
						{item.label}
					</a>{/each}
			</nav>
			<div class="side-bottom">
				<div class="status-dot"></div>
				<span>媒体服务</span>
				<span class="status-live">自托管</span>
			</div>
		</aside>
		<main>
			<header class="topbar">
				<div class="breadcrumb">
					个人直播档案 <span>/</span>
					{links.find((item) => page.url.pathname.startsWith(item.href))?.label ??
						(page.url.pathname === '/live' ? '频道详情' : '频道列表')}
				</div>
				<div class="auth">
					<span class="auth-state connected">{savedRole === 'viewer' ? '只读访问' : '管理员'}</span>
					<button class="button secondary small" type="button" onclick={clearKey}>退出登录</button>
				</div>
			</header>
			<div class="content">{@render children()}</div>
		</main>
	</div>
{/if}

<style>
	:global(*) {
		box-sizing: border-box;
	}
	:global(:root) {
		font-family:
			Inter,
			-apple-system,
			BlinkMacSystemFont,
			'Segoe UI',
			sans-serif;
		color: #202938;
		background: #f3f5f8;
		font-synthesis: none;
		text-rendering: optimizeLegibility;
		--ink: #202938;
		--muted: #697586;
		--line: #e3e8ef;
		--surface: #fff;
		--canvas: #f3f5f8;
		--accent: #6653c7;
		--accent-soft: #f0edff;
		--green: #16835f;
	}
	:global(body) {
		margin: 0;
		background: var(--canvas);
		color: var(--ink);
		font:
			14px/1.55 Inter,
			-apple-system,
			BlinkMacSystemFont,
			'Segoe UI',
			sans-serif;
	}
	:global(button),
	:global(input),
	:global(select) {
		font: inherit;
	}
	:global(button) {
		cursor: pointer;
	}
	:global(button:focus-visible),
	:global(a:focus-visible),
	:global(input:focus-visible),
	:global(select:focus-visible) {
		outline: 3px solid #a99afa;
		outline-offset: 2px;
	}
	.app-shell {
		min-height: 100vh;
		display: grid;
		grid-template-columns: 252px minmax(0, 1fr);
	}
	.sidebar {
		position: sticky;
		top: 0;
		height: 100vh;
		background: #171b29;
		border-right: 1px solid #252b3b;
		padding: 27px 17px 19px;
		display: flex;
		flex-direction: column;
	}
	.brand {
		display: flex;
		align-items: center;
		gap: 12px;
		color: #fff;
		text-decoration: none;
		font-weight: 720;
		font-size: 17px;
		margin: 0 7px 49px;
		letter-spacing: -0.25px;
	}
	.brand small {
		display: block;
		color: #9ca4b7;
		font-size: 9px;
		letter-spacing: 1.55px;
		font-weight: 650;
		margin-top: 2px;
	}
	.brand-mark {
		width: 37px;
		height: 37px;
		border-radius: 12px;
		display: grid;
		place-items: center;
		background: linear-gradient(145deg, #a493ff, #6955c9);
		font-size: 20px;
		color: white;
		box-shadow: 0 7px 18px #6755c944;
	}
	.nav-label {
		padding: 0 12px;
		color: #7e879d;
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 1.35px;
		font-weight: 700;
		margin-bottom: 11px;
	}
	nav {
		display: grid;
		gap: 5px;
	}
	nav a {
		display: flex;
		gap: 12px;
		align-items: center;
		padding: 11px 12px;
		border-radius: 9px;
		color: #aeb6c8;
		text-decoration: none;
		font-size: 13px;
		font-weight: 530;
		transition:
			background 0.15s,
			color 0.15s;
	}
	nav a span {
		font-size: 16px;
		width: 19px;
		text-align: center;
		color: #8f99b1;
	}
	nav a.active,
	nav a:hover {
		background: #262b3c;
		color: #fff;
	}
	nav a.active {
		box-shadow: inset 3px 0 #a493ff;
	}
	nav a.active span {
		color: #bdadff;
	}
	.side-bottom {
		margin-top: auto;
		border-top: 1px solid #303547;
		padding: 16px 7px 0;
		display: flex;
		align-items: center;
		gap: 9px;
		font-size: 11px;
		color: #a2aabd;
	}
	.status-dot {
		height: 7px;
		width: 7px;
		border-radius: 100%;
		background: #45c997;
		box-shadow: 0 0 9px #38cf9755;
	}
	.status-live {
		margin-left: auto;
		color: #6ad6ae;
		font-size: 10px;
	}
	main {
		min-width: 0;
	}
	.topbar {
		min-height: 68px;
		border-bottom: 1px solid var(--line);
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 20px;
		padding: 0 clamp(22px, 3vw, 48px);
		background: #fff;
	}
	.breadcrumb {
		color: #788394;
		font-size: 12px;
	}
	.breadcrumb span {
		color: #b3bdca;
		margin: 0 9px;
	}
	.auth {
		display: flex;
		align-items: center;
		gap: 9px;
	}
	.auth-state {
		font-size: 11px;
		color: #a66b1e;
		font-weight: 600;
	}
	.auth-state.connected {
		color: var(--green);
	}
	.content {
		padding: 34px clamp(22px, 3vw, 48px) 48px;
		max-width: 1580px;
		margin: 0 auto;
		width: 100%;
	}
	.button {
		border: 1px solid var(--accent);
		color: white;
		background: var(--accent);
		border-radius: 8px;
		padding: 9px 14px;
		font-weight: 650;
		font-size: 12px;
		line-height: 1.3;
		box-shadow: 0 1px 2px #1d174010;
		transition:
			background 0.15s,
			border-color 0.15s,
			transform 0.15s;
	}
	.button:hover {
		background: #5845b7;
		border-color: #5845b7;
	}
	.button:active {
		transform: translateY(1px);
	}
	.button:disabled {
		opacity: 0.48;
		cursor: not-allowed;
	}
	.button.secondary {
		border-color: #dce2ea;
		background: #fff;
		color: #4b5668;
		box-shadow: none;
	}
	.button.secondary:hover {
		background: #f5f6fa;
		border-color: #c9d0db;
		color: #293346;
	}
	.button.small {
		padding: 7px 11px;
		font-size: 11px;
	}
	/* Shared visual language for route-level components. */
	:global(.content h1) {
		font-size: 30px;
		line-height: 1.2;
		letter-spacing: -0.7px;
		color: #202938;
		margin: 7px 0 7px;
		font-weight: 720;
	}
	:global(.content h2) {
		color: #263143;
		font-size: 16px;
		letter-spacing: -0.2px;
		font-weight: 680;
	}
	:global(.content p) {
		color: #758194;
		font-size: 13px;
		line-height: 1.6;
	}
	:global(.content .eyebrow) {
		font-size: 10px;
		letter-spacing: 1.45px;
		color: #7663c9;
		font-weight: 750;
	}
	:global(.content .panel) {
		background: #fff;
		border: 1px solid #e3e8ef;
		border-radius: 13px;
		box-shadow: 0 2px 7px #26354a08;
	}
	:global(.content section.panel) {
		background: #fff;
		border-color: #e3e8ef;
		border-radius: 13px;
		box-shadow: 0 2px 7px #26354a08;
	}
	:global(.content aside.panel) {
		display: flex;
		flex-direction: column;
		background: #fff;
		border: 1px solid #e3e8ef;
		border-radius: 13px;
		box-shadow: 0 2px 7px #26354a08;
	}
	:global(.content .button) {
		border-radius: 8px;
		font-size: 12px;
		padding: 9px 14px;
	}
	:global(.content .button.secondary) {
		border-color: #dfe4eb;
		background: #fff;
		color: #4b5668;
	}
	:global(.content input:not([type='checkbox']), .content select) {
		color: #273247;
		background: #fbfcfd;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 9px 11px;
		min-height: 38px;
		transition:
			border-color 0.15s,
			box-shadow 0.15s;
	}
	:global(.content input:not([type='checkbox']):focus, .content select:focus) {
		border-color: #9988ed;
		box-shadow: 0 0 0 3px #7663d51a;
		outline: 0;
	}
	:global(.content label) {
		color: #667286;
		font-size: 12px;
		font-weight: 570;
	}
	:global(.content .error) {
		color: #a63242;
		background: #fff2f3;
		border: 1px solid #f4d6da;
		border-radius: 9px;
		padding: 11px 14px;
	}
	:global(.content .empty) {
		color: #7a8596;
		background: #fafbfd;
		border: 1px dashed #dce2ea;
		border-radius: 10px;
		padding: 32px 20px;
		text-align: center;
	}
	:global(.content .form.panel),
	:global(.content .filters.panel) {
		padding: 22px;
		border-radius: 13px;
		gap: 16px;
	}
	:global(.content .form label),
	:global(.content .filters label) {
		display: grid;
		align-content: end;
		gap: 7px;
		font-size: 12px;
		color: #657185;
	}
	:global(
		.content .form input:not([type='checkbox']),
		.content .form select,
		.content .filters input,
		.content .filters select
	) {
		width: 100%;
		min-width: 0;
		background: #fbfcfd;
		color: #273247;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 9px 11px;
		min-height: 40px;
	}
	:global(.content .fields input[type='number']) {
		width: 180px;
		background: #fbfcfd;
		color: #273247;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 9px 11px;
		min-height: 40px;
	}
	:global(.content .cards .card) {
		padding: 18px;
		gap: 9px;
	}
	:global(.content .cards .card small) {
		font-size: 11px;
		color: #778397;
		font-weight: 600;
	}
	:global(.content .cards .card strong) {
		font-size: 25px;
		line-height: 1.2;
		letter-spacing: -0.5px;
		color: #283448;
	}
	:global(.content .cards .card strong.green) {
		color: #16835f;
	}
	:global(.content .cards .card strong.waiting) {
		color: #a66b1e;
	}
	:global(.content .cards .card span) {
		font-size: 11px;
		line-height: 1.45;
		color: #748095;
	}
	:global(.content .stream-row) {
		color: #283448;
		border-bottom-color: #edf0f4;
		padding: 13px 15px;
	}
	:global(.content aside.panel h2) {
		color: #263143;
	}
	:global(.content aside.panel .stream-list) {
		flex: 1;
		border-top-color: #edf0f4;
	}
	:global(.content .video-top select) {
		color: #344054;
		background: #fbfcfd;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 8px 10px;
		min-height: 36px;
	}
	:global(.content .mode) {
		color: #738095;
		border-radius: 7px;
		padding: 8px 10px;
	}
	:global(.content .mode.chosen) {
		color: #5743b1;
		background: #f0edff;
	}
	:global(.content .notice) {
		border: 1px solid #f1dfbd;
		background: #fff8eb;
		color: #875719;
		padding: 11px 14px;
		border-radius: 9px;
		margin-bottom: 15px;
		font-size: 12px;
	}
	:global(.content .stream-row:hover, .content .stream-row.selected-row) {
		background: #f7f6ff;
	}
	:global(.content .stream-icon) {
		background: #f0edff;
		color: #6955c9;
	}
	:global(.content .stream-copy small) {
		color: #7e899a;
	}
	:global(.content .offline) {
		color: #758195;
	}
	:global(.content .offline.online) {
		color: #16835f;
	}
	:global(.content .count) {
		background: #f1f3f7;
		color: #667286;
	}
	:global(.content .stream-control) {
		background: #f8f9fb;
		border-color: #e5e9ef;
		color: #667286;
	}
	:global(.content .stream-control strong) {
		color: #4f3ca8;
	}
	:global(.content .stream-control small) {
		color: #798496;
	}
	:global(.content .add-row input) {
		background: #fbfcfd;
		color: #273247;
		border-color: #dce2ea;
		min-height: 38px;
	}
	:global(.content .panel-title .eyebrow) {
		color: #7663c9;
	}
	:global(.content .list-empty) {
		display: grid;
		place-items: center;
		min-height: 180px;
		color: #778397;
		text-align: center;
	}
	:global(.content .job) {
		border-top-color: #edf0f4;
	}
	:global(.content .panel h2) {
		color: #263143;
	}
	:global(.content .jobs.panel) {
		padding: 20px;
	}
	:global(.content .title) {
		margin-bottom: 14px;
	}
	:global(.content .title h2) {
		margin: 0;
	}
	:global(.content .job-mark) {
		background: #f0edff;
		color: #6955c9;
	}
	:global(.content .job-main small, .content .progress small) {
		color: #778397;
		font-size: 11px;
	}
	:global(.content .progress > div) {
		background: #edf0f4;
	}
	:global(.content .progress i) {
		background: linear-gradient(90deg, #6b57ca, #9c8be8);
	}
	:global(.content .timeline-head small) {
		color: #778397;
	}
	:global(.content .bucket) {
		background: #f9fafc;
		border-color: #e4e8ef;
		border-radius: 9px;
		padding: 12px;
	}
	:global(.content .bucket:hover) {
		border-color: #9988ed;
		background: #f6f4ff;
	}
	:global(.content .bucket span) {
		font-size: 11px;
		color: #344054;
		font-weight: 620;
	}
	:global(.content .bucket small) {
		font-size: 10px;
		color: #778397;
	}
	:global(.content .bucket i) {
		background: #e9edf3;
	}
	:global(.content .bucket b) {
		background: linear-gradient(90deg, #6955c9, #a595ef);
	}
	:global(.content .legend) {
		color: #687487;
		font-size: 11px;
	}
	:global(.content .range-dot.good) {
		background: #37b987;
	}
	:global(.content .row) {
		border-color: #edf0f4;
	}
	:global(.content .row strong) {
		color: #344054;
	}
	:global(.content .row small) {
		color: #778397;
	}
	:global(.content .row time) {
		color: #687487;
	}
	:global(.content .info p) {
		color: #758194;
	}
	.auth-loading {
		min-height: 100vh;
		display: flex;
		align-items: center;
		justify-content: center;
		gap: 12px;
		color: #687487;
		font-size: 13px;
	}
	.loading-mark {
		width: 36px;
		height: 36px;
		border-radius: 11px;
		display: grid;
		place-items: center;
		background: linear-gradient(145deg, #a493ff, #6955c9);
		font-size: 19px;
		font-weight: 700;
		color: white;
		box-shadow: 0 7px 18px #6755c944;
	}
	@media (max-width: 900px) {
		.app-shell {
			grid-template-columns: 218px minmax(0, 1fr);
		}
		.sidebar {
			padding-left: 13px;
			padding-right: 13px;
		}
		.topbar {
			padding: 0 22px;
		}
		.content {
			padding: 27px 22px 40px;
		}
	}
	@media (max-width: 720px) {
		.app-shell {
			grid-template-columns: minmax(0, 1fr);
		}
		.sidebar {
			position: relative;
			height: auto;
			padding: 12px 14px 10px;
			border-right: 0;
			border-bottom: 1px solid #252b3b;
		}
		.brand {
			margin: 0 4px 10px;
		}
		.nav-label,
		.side-bottom {
			display: none;
		}
		nav {
			display: flex;
			overflow-x: auto;
			gap: 4px;
			padding-bottom: 1px;
		}
		nav a {
			white-space: nowrap;
			font-size: 12px;
			padding: 9px 11px;
		}
		.topbar {
			display: grid;
			grid-template-columns: minmax(0, 1fr);
			min-width: 0;
			min-height: 58px;
			padding: 9px 15px;
			gap: 7px;
		}
		.breadcrumb {
			display: none;
		}
		.auth {
			display: flex;
			width: 100%;
			min-width: 0;
			justify-content: flex-end;
			gap: 7px;
		}
		.auth-state {
			margin-right: auto;
			white-space: nowrap;
		}
		.content {
			padding: 22px 15px 34px;
		}
		:global(.content h1) {
			font-size: 26px;
		}
	}
</style>
