<script lang="ts">
	import { userFacingError } from '#lib/messages';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { loginWithToken } from '#lib/auth';
	import { notifyAuthChange } from '#lib/events';

	let token = $state('');
	let remember = $state(true);
	let busy = $state(false);
	let error = $state('');
	const destination = () => {
		const next = page.url.searchParams.get('next');
		return next?.startsWith('/') && !next.startsWith('//') && !next.startsWith('/login')
			? next
			: '/channels';
	};

	async function submit() {
		busy = true;
		error = '';
		try {
			const session = await loginWithToken(token, remember);
			notifyAuthChange(session);
			await goto(destination());
		} catch (cause) {
			error = userFacingError(
				cause,
				cause instanceof TypeError
					? '无法连接媒体服务，请检查服务状态后重试。'
					: '登录失败，请检查访问令牌后重试。',
			);
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head>
	<meta name="description" content="登录 Backtrack 私人录像服务" />
</svelte:head>

<main class="login-shell">
	<div class="login-glow glow-one"></div>
	<div class="login-glow glow-two"></div>
	<section class="login-card">
		<div class="brand-lockup">
			<img class="brand-mark" src="/icon.svg" alt="" />
			<div>
				<strong>Backtrack</strong>
				<small>私人直播档案</small>
			</div>
		</div>

		<div class="login-copy">
			<div class="eyebrow">私人直播档案</div>
			<h1>欢迎回来</h1>
			<p>使用服务访问令牌登录，继续管理频道、直播和录像。</p>
		</div>

		<form
			onsubmit={(event) => {
				event.preventDefault();
				void submit();
			}}
		>
			<label for="access-token">访问令牌</label>
			<input
				id="access-token"
				type="password"
				bind:value={token}
				placeholder="请输入访问令牌"
				autocomplete="current-password"
				required
			/>
			<label class="remember-row">
				<input type="checkbox" bind:checked={remember} />
				<span>在此设备上记住我</span>
			</label>
			{#if error}<p class="form-error" role="alert">{error}</p>{/if}
			<button type="submit" disabled={busy || !token.trim()}>
				{busy ? '正在登录…' : '登录工作区'}
				<span aria-hidden="true">→</span>
			</button>
		</form>

		<div class="privacy-note">
			<span class="lock-icon">⌑</span>
			<span>
				启用“记住我”后，令牌会保存在此浏览器以便下次自动登录。请勿在公用设备上启用此选项。
			</span>
		</div>
	</section>
	<footer>私人部署 · 录像由你掌控</footer>
</main>

<style>
	.login-shell {
		position: relative;
		isolation: isolate;
		min-height: 100vh;
		display: grid;
		place-items: center;
		overflow: hidden;
		padding: 32px 18px;
		background: radial-gradient(ellipse at 50% 12%, #f9f8ff 0, #f4f5f9 48%, #edf0f6 100%);
	}
	.login-card {
		position: relative;
		z-index: 1;
		width: min(100%, 448px);
		padding: 38px 40px 30px;
		border: 1px solid #e3e7ef;
		border-radius: 20px;
		background: #fff;
		box-shadow:
			0 24px 70px #29375314,
			0 3px 9px #29375308;
	}
	.brand-lockup {
		display: flex;
		align-items: center;
		gap: 12px;
		margin-bottom: 43px;
		color: #202938;
	}
	.brand-lockup strong {
		display: block;
		font-size: 17px;
		line-height: 1.2;
		letter-spacing: -0.3px;
	}
	.brand-lockup small {
		display: block;
		margin-top: 4px;
		color: #8993a6;
		font-size: 9px;
		font-weight: 700;
		letter-spacing: 1.6px;
	}
	.brand-mark {
		width: 39px;
		height: 39px;
		object-fit: contain;
	}
	.eyebrow {
		color: #7663c9;
		font-size: 10px;
		font-weight: 750;
		letter-spacing: 1.45px;
	}
	.login-copy h1 {
		margin: 8px 0 8px;
		color: #202938;
		font-size: 29px;
		letter-spacing: -0.8px;
		line-height: 1.2;
	}
	.login-copy p {
		margin: 0;
		color: #758194;
		font-size: 13px;
		line-height: 1.6;
	}
	form {
		display: grid;
		gap: 9px;
		margin-top: 28px;
	}
	form > label:first-child {
		color: #344054;
		font-size: 12px;
		font-weight: 650;
	}
	input[type='password'] {
		width: 100%;
		height: 46px;
		border: 1px solid #dce2ea;
		border-radius: 9px;
		padding: 0 13px;
		background: #fbfcfd;
		color: #273247;
		font-size: 13px;
		transition:
			border-color 0.15s,
			box-shadow 0.15s;
	}
	input[type='password']:focus {
		border-color: #9988ed;
		box-shadow: 0 0 0 3px #7663d51a;
		outline: 0;
	}
	input::placeholder {
		color: #a1aaba;
	}
	.remember-row {
		display: flex;
		align-items: center;
		gap: 9px;
		margin: 5px 0 4px;
		color: #667286;
		font-size: 12px;
		cursor: pointer;
	}
	.remember-row input {
		width: 15px;
		height: 15px;
		margin: 0;
		accent-color: #6955c9;
	}
	form button {
		display: flex;
		align-items: center;
		justify-content: center;
		gap: 10px;
		height: 46px;
		margin-top: 4px;
		border: 1px solid #6653c7;
		border-radius: 9px;
		background: #6653c7;
		color: #fff;
		font-size: 13px;
		font-weight: 680;
		transition:
			background 0.15s,
			transform 0.15s;
	}
	form button:hover:not(:disabled) {
		background: #5845b7;
	}
	form button:active:not(:disabled) {
		transform: translateY(1px);
	}
	form button:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	form button span {
		font-size: 17px;
		font-weight: 400;
	}
	.form-error {
		margin: 0;
		padding: 10px 12px;
		border: 1px solid #f1d5d9;
		border-radius: 8px;
		background: #fff3f4;
		color: #a63242;
		font-size: 12px;
		line-height: 1.45;
	}
	.privacy-note {
		display: flex;
		align-items: flex-start;
		gap: 9px;
		margin-top: 22px;
		padding-top: 17px;
		border-top: 1px solid #edf0f4;
		color: #8792a3;
		font-size: 11px;
		line-height: 1.55;
	}
	.lock-icon {
		display: grid;
		place-items: center;
		flex: 0 0 20px;
		height: 20px;
		border-radius: 6px;
		background: #f1efff;
		color: #705cc7;
		font-size: 14px;
	}
	.login-glow {
		position: absolute;
		z-index: 0;
		width: 400px;
		height: 400px;
		border-radius: 50%;
		filter: blur(90px);
		opacity: 0.32;
		pointer-events: none;
	}
	.glow-one {
		top: -260px;
		left: calc(50% - 330px);
		background: #d8d0ff;
	}
	.glow-two {
		right: calc(50% - 510px);
		bottom: -330px;
		background: #d3e2ff;
	}
	footer {
		position: absolute;
		bottom: 20px;
		color: #98a2b1;
		font-size: 10px;
		letter-spacing: 0.3px;
	}
	@media (max-width: 520px) {
		.login-shell {
			padding: 20px 14px 52px;
		}
		.login-card {
			padding: 30px 23px 23px;
			border-radius: 17px;
		}
		.brand-lockup {
			margin-bottom: 35px;
		}
		.login-copy h1 {
			font-size: 27px;
		}
		footer {
			bottom: 16px;
		}
	}
</style>
