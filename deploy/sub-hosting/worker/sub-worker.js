/**
 * sbx 订阅分发 Worker
 *
 * 职责：把「私有仓库里的订阅文件」变成一个带令牌校验的 HTTPS 端点。
 * 仓库保持 Private，PAT 只读单仓库 —— 即便 Worker 配置泄漏，也拿不到面板。
 *
 * 变量（wrangler.toml / Dashboard → Settings → Variables）：
 *   REPO          owner/repo
 *   BRANCH        默认 main
 *   PATH_SUB      默认 sub.txt
 *   PATH_SINGBOX  默认 sub-singbox.json
 * 密钥（wrangler secret put）：
 *   SUB_TOKEN     分发令牌，openssl rand -hex 32
 *   GH_TOKEN      fine-grained PAT，Contents: Read-only
 *
 * 刻意不做的事：
 *   - 不 console.log 请求 URL：路径里就是令牌，日志会原样记下来。
 *   - 不用 Cache API 自定义缓存键：缓存键一旦是某个可被外部请求的路径，
 *     就可能被 CDN 直接从边缘返回，绕过 Worker 的令牌校验。
 *     这里只依赖响应头 Cache-Control + 上游 cf.cacheTtl，缓存键就是带令牌的
 *     客户端 URL，拿不到令牌就取不到缓存，语义正确。
 */

const CACHE_TTL = 300;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    if (request.method !== 'GET' && request.method !== 'HEAD') {
      return text('method not allowed', 405);
    }

    const matched = url.pathname.match(/^\/sub\/([A-Za-z0-9_-]{16,128})\/?$/);
    if (!matched) return text('not found', 404);

    // 与 sbx 面板同一条规则：令牌错也回 404，不告诉扫描者「路径存在、只是密码不对」。
    if (!safeEqual(matched[1], env.SUB_TOKEN || '')) {
      return text('not found', 404);
    }

    const wantSingbox = url.searchParams.get('target') === 'singbox';
    const file = wantSingbox
      ? (env.PATH_SINGBOX || 'sub-singbox.json')
      : (env.PATH_SUB || 'sub.txt');

    const upstream = await fetch(
      `https://raw.githubusercontent.com/${env.REPO}/${env.BRANCH || 'main'}/${file}`,
      {
        headers: {
          Authorization: `Bearer ${env.GH_TOKEN}`,
          'User-Agent': 'sbx-sub-worker',
          Accept: 'text/plain, application/json',
        },
        // 上游是 raw.githubusercontent.com，不走这个 TTL 就没有任何缓存，
        // 每个客户端请求都会打一次 GitHub。
        cf: { cacheTtl: CACHE_TTL, cacheEverything: true },
      }
    );

    if (!upstream.ok) {
      // 502 而不是 404：让客户端知道是上游出了问题，而不是「订阅被删了」。
      return text(`upstream ${upstream.status}`, 502);
    }

    const body = await upstream.text();
    if (!body) return text('empty upstream', 502);

    return new Response(body, {
      headers: {
        'Content-Type': wantSingbox
          ? 'application/json; charset=utf-8'
          : 'text/plain; charset=utf-8',
        'Cache-Control': `public, max-age=${CACHE_TTL}`,
        'X-Robots-Tag': 'noindex, nofollow',
        'X-Content-Type-Options': 'nosniff',
      },
    });
  },
};

function text(body, status) {
  return new Response(body, {
    status,
    headers: {
      'Content-Type': 'text/plain; charset=utf-8',
      'X-Robots-Tag': 'noindex, nofollow',
    },
  });
}

/** 常量时间比较，避免按字符提前返回泄漏前缀。长度本身不敏感（固定 64 位）。 */
function safeEqual(a, b) {
  if (typeof a !== 'string' || typeof b !== 'string' || a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i += 1) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}
