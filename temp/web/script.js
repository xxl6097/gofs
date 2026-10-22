/* ============================================================
   NEON LAB · 交互脚本
   在 <head> 中同步加载：
   1) 第一时间恢复主题，避免刷新闪色；
   2) 等 DOM 解析完成后再初始化画布与交互。
   ============================================================ */
(() => {
  'use strict';

  /* ---------- 主题：默认暗色，记住上次选择 ---------- */
  const root = document.documentElement;
  try {
    const saved = localStorage.getItem('neon-theme');
    if (saved) root.dataset.theme = saved;
  } catch (e) {}
  root.classList.remove('no-js');
  root.classList.add('js');

  /* ---------- DOM 就绪后初始化页面 ---------- */
  function boot() {
    /* ==========================================================
       工具函数
       ========================================================== */
    const TAU = Math.PI * 2;
    const clamp = (v, a, b) => Math.min(b, Math.max(a, v));
    const lerp = (a, b, t) => a + (b - a) * t;
    const rand = (a, b) => a + Math.random() * (b - a);
    const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    const isLight = () => root.dataset.theme === 'light';
    const cssVar = (name) =>
      getComputedStyle(root).getPropertyValue(name).trim();

    /* ==========================================================
       主题切换
       ========================================================== */
    const themeBtn = document.getElementById('themeToggle');
    themeBtn.addEventListener('click', () => {
      root.dataset.theme = isLight() ? 'dark' : 'light';
      try { localStorage.setItem('neon-theme', root.dataset.theme); } catch (e) {}
      readPalette();
    });

    /* ==========================================================
       画布尺寸工具（自动处理 DPR）
       ========================================================== */
    function setupCanvas(canvas) {
      const ctx = canvas.getContext('2d');
      let w = 1, h = 1;
      return {
        ctx,
        get w() { return w; },
        get h() { return h; },
        resize() {
          const rect = canvas.getBoundingClientRect();
          const dpr = Math.min(window.devicePixelRatio || 1, 2);
          w = Math.max(1, rect.width);
          h = Math.max(1, rect.height);
          canvas.width = Math.round(w * dpr);
          canvas.height = Math.round(h * dpr);
          ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        }
      };
    }

    /* ==========================================================
       背景：星尘 + 流星 + 光标视差
       ========================================================== */
    const bgCanvas = document.getElementById('bg');
    const bg = setupCanvas(bgCanvas);
    let stars = [];
    let meteors = [];
    let palette = { star: '#ffffff' };
    let pointer = { x: 0.5, y: 0.5, tx: 0.5, ty: 0.5 };

    function readPalette() {
      palette.star = cssVar('--star') || '#ffffff';
    }

    function initStars() {
      const count = Math.round(clamp((bg.w * bg.h) / 16000, 40, 150));
      stars = Array.from({ length: count }, () => ({
        x: Math.random() * bg.w,
        y: Math.random() * bg.h,
        r: rand(0.4, 1.7),
        a: rand(0.15, 0.75),
        t: rand(0, TAU),
        tw: rand(0.4, 1.6),
        drift: rand(0.005, 0.03),
        depth: rand(0.25, 1)
      }));
    }

    function initMeteors() {
      meteors = [];
    }

    function drawBackground(now) {
      const ctx = bg.ctx;
      const w = bg.w, h = bg.h;
      ctx.clearRect(0, 0, w, h);

      pointer.x = lerp(pointer.x, pointer.tx, 0.05);
      pointer.y = lerp(pointer.y, pointer.ty, 0.05);
      const px = (pointer.x - 0.5) * 2;
      const py = (pointer.y - 0.5) * 2;
      const light = isLight();

      ctx.save();
      ctx.fillStyle = palette.star;

      for (const s of stars) {
        s.t += 0.016 * s.tw;
        s.y += s.drift * s.depth;
        if (s.y > h + 2) { s.y = -2; s.x = Math.random() * w; }
        const alpha = s.a * (0.55 + 0.45 * Math.sin(s.t)) * (light ? 0.35 : 1);
        ctx.globalAlpha = clamp(alpha, 0, 1);
        ctx.beginPath();
        ctx.arc(s.x + px * 22 * s.depth, s.y + py * 22 * s.depth, s.r, 0, TAU);
        ctx.fill();
      }
      ctx.restore();

      /* 流星 */
      if (meteors.length < 3 && Math.random() < 0.004) {
        meteors.push({
          x: rand(w * 0.1, w * 1.15),
          y: rand(-60, h * 0.35),
          vx: rand(-11, -6),
          vy: rand(5, 9),
          len: rand(90, 200),
          life: 1
        });
      }
      for (let i = meteors.length - 1; i >= 0; i--) {
        const m = meteors[i];
        m.x += m.vx;
        m.y += m.vy;
        m.life -= 0.008;
        if (m.life <= 0 || m.y > h + 220) { meteors.splice(i, 1); continue; }
        const nx = m.x - m.vx * (m.len / 8);
        const ny = m.y - m.vy * (m.len / 8);
        const grad = ctx.createLinearGradient(m.x, m.y, nx, ny);
        grad.addColorStop(0, `rgba(180, 240, 255, ${0.85 * m.life})`);
        grad.addColorStop(1, 'rgba(180, 240, 255, 0)');
        ctx.strokeStyle = grad;
        ctx.lineWidth = 1.6;
        ctx.beginPath();
        ctx.moveTo(m.x, m.y);
        ctx.lineTo(nx, ny);
        ctx.stroke();
      }
    }

    /* ==========================================================
       FPS 计数
       ========================================================== */
    const fpsEl = document.getElementById('fps');
    let fpsFrames = 0;
    let fpsLast = performance.now();

    function tickFps(now) {
      fpsFrames++;
      if (now - fpsLast >= 500) {
        const fps = Math.round((fpsFrames * 1000) / (now - fpsLast));
        fpsEl.textContent = (fps > 0 ? fps : 0) + ' FPS';
        fpsFrames = 0;
        fpsLast = now;
      }
    }

    /* ==========================================================
       粒子游乐场
       ========================================================== */
    const stage = document.getElementById('stage');
    const stageHint = document.getElementById('stageHint');
    const pc = setupCanvas(document.getElementById('particles'));
    const countRange = document.getElementById('countRange');
    const speedRange = document.getElementById('speedRange');
    const hueRange = document.getElementById('hueRange');
    const countOut = document.getElementById('countOut');
    const speedOut = document.getElementById('speedOut');
    const hueOut = document.getElementById('hueOut');
    const pcountEl = document.getElementById('pcount');
    const pauseBtn = document.getElementById('pauseBtn');
    const resetBtn = document.getElementById('resetBtn');

    const sim = {
      mode: 'repel',
      count: 120,
      speed: 1,
      hue: 188,
      paused: false,
      pointer: { x: 0, y: 0, active: false, down: false }
    };
    let parts = [];

    function makeParticles(n) {
      return Array.from({ length: n }, () => ({
        x: rand(0, pc.w),
        y: rand(0, pc.h),
        vx: rand(-0.35, 0.35),
        vy: rand(-0.35, 0.35),
        r: rand(1.2, 3.1),
        hueShift: rand(-20, 20)
      }));
    }

    function refreshParticles() {
      parts = makeParticles(sim.count);
      pcountEl.textContent = sim.count + ' 粒子';
    }

    function updateParticle(dt) {
      const n = parts.length;
      const mode = sim.mode;
      const mouse = sim.pointer;
      const R = 170;
      const R2 = R * R;
      const power = mouse.down ? 2.2 : 1;
      const light = isLight();

      for (let i = 0; i < n; i++) {
        const p = parts[i];

        if (mode !== 'none' && mouse.active) {
          const dx = p.x - mouse.x;
          const dy = p.y - mouse.y;
          const d2 = dx * dx + dy * dy;
          if (d2 < R2 && d2 > 0.01) {
            const d = Math.sqrt(d2);
            const falloff = 1 - d / R;

            if (mode === 'repel') {
              const f = falloff * falloff * 16 * power;
              p.vx += (dx / d) * f * dt;
              p.vy += (dy / d) * f * dt;
            } else if (mode === 'attract') {
              const f = falloff * 4.2 * power;
              p.vx -= (dx / d) * f * dt;
              p.vy -= (dy / d) * f * dt;
              /* 切向分量，让粒子绕圈而不是撞成一团 */
              p.vx += (-dy / d) * falloff * 3.4 * dt;
              p.vy += (dx / d) * falloff * 3.4 * dt;
            } else if (mode === 'vortex') {
              const f = falloff * 5.4 * power;
              p.vx += (-dy / d) * f * dt;
              p.vy += (dx / d) * f * dt;
              p.vx -= (dx / d) * falloff * 1.1 * dt;
              p.vy -= (dy / d) * falloff * 1.1 * dt;
            }
          }
        }

        /* 阻尼 + 限速 */
        p.vx *= 0.985;
        p.vy *= 0.985;
        const sp = Math.hypot(p.vx, p.vy);
        const maxSp = 5;
        if (sp > maxSp) {
          p.vx = (p.vx / sp) * maxSp;
          p.vy = (p.vy / sp) * maxSp;
        }

        p.x += p.vx * sim.speed * dt * 1.5;
        p.y += p.vy * sim.speed * dt * 1.5;

        /* 环绕边界 */
        if (p.x < -30) p.x = pc.w + 30;
        else if (p.x > pc.w + 30) p.x = -30;
        if (p.y < -30) p.y = pc.h + 30;
        else if (p.y > pc.h + 30) p.y = -30;
      }

      /* ---- 绘制 ---- */
      const ctx = pc.ctx;
      ctx.clearRect(0, 0, pc.w, pc.h);

      /* 连线：合成单条路径，一次 stroke，省性能 */
      const LINK = pc.w < 520 ? 88 : 118;
      const LINK2 = LINK * LINK;
      ctx.beginPath();
      for (let i = 0; i < n; i++) {
        const a = parts[i];
        for (let j = i + 1; j < n; j++) {
          const b = parts[j];
          const dx = a.x - b.x;
          const dy = a.y - b.y;
          if (dx * dx + dy * dy < LINK2) {
            ctx.moveTo(a.x, a.y);
            ctx.lineTo(b.x, b.y);
          }
        }
      }
      ctx.lineWidth = 1;
      ctx.strokeStyle = `hsla(${sim.hue}, 95%, ${light ? 40 : 62}%, ${light ? 0.16 : 0.11})`;
      ctx.stroke();

      /* 粒子本体：外圈辉光 + 内核 */
      if (!light) ctx.globalCompositeOperation = 'lighter';
      for (let i = 0; i < n; i++) {
        const p = parts[i];
        const hue = (sim.hue + p.hueShift + 360) % 360;
        ctx.beginPath();
        ctx.arc(p.x, p.y, p.r * 3.4, 0, TAU);
        ctx.fillStyle = `hsla(${hue}, 100%, 62%, ${light ? 0.05 : 0.07})`;
        ctx.fill();
        ctx.beginPath();
        ctx.arc(p.x, p.y, p.r, 0, TAU);
        ctx.fillStyle = `hsla(${hue}, 100%, ${light ? 45 : 74}%, ${light ? 0.8 : 0.95})`;
        ctx.fill();
      }
      ctx.globalCompositeOperation = 'source-over';

      /* 鼠标力场提示 */
      if (mouse.active && sim.mode !== 'none') {
        ctx.beginPath();
        ctx.arc(mouse.x, mouse.y, R, 0, TAU);
        ctx.strokeStyle = `hsla(${sim.hue}, 100%, 65%, ${mouse.down ? 0.28 : 0.14})`;
        ctx.setLineDash([3, 7]);
        ctx.stroke();
        ctx.setLineDash([]);
      }
    }

    /* 主循环：dt 归一化到 60fps 的一帧 */
    let lastTime = performance.now();
    function loop(now) {
      const dt = clamp((now - lastTime) / 16.667, 0.2, 2.4);
      lastTime = now;
      if (!sim.paused && document.visibilityState === 'visible') {
        updateParticle(dt);
        tickFps(now);
      }
      requestAnimationFrame(loop);
    }

    /* 指针交互 */
    function setPointer(e) {
      const rect = pcCanvasRect();
      sim.pointer.x = e.clientX - rect.left;
      sim.pointer.y = e.clientY - rect.top;
      sim.pointer.active = true;
    }
    const pcCanvas = document.getElementById('particles');
    function pcCanvasRect() { return pcCanvas.getBoundingClientRect(); }

    pcCanvas.addEventListener('pointermove', (e) => {
      setPointer(e);
      stageHint.classList.add('hide');
    });
    pcCanvas.addEventListener('pointerdown', (e) => {
      setPointer(e);
      sim.pointer.down = true;
      if (pcCanvas.setPointerCapture) pcCanvas.setPointerCapture(e.pointerId);
      stageHint.classList.add('hide');
    });
    pcCanvas.addEventListener('pointerup', () => { sim.pointer.down = false; });
    pcCanvas.addEventListener('pointercancel', () => {
      sim.pointer.down = false;
      sim.pointer.active = false;
    });
    pcCanvas.addEventListener('pointerleave', () => {
      sim.pointer.active = false;
      sim.pointer.down = false;
    });

    /* 控件 */
    document.querySelectorAll('.segmented button').forEach((btn) => {
      btn.addEventListener('click', () => {
        sim.mode = btn.dataset.mode;
        document.querySelectorAll('.segmented button').forEach((b) => {
          b.classList.remove('active');
          b.setAttribute('aria-pressed', 'false');
        });
        btn.classList.add('active');
        btn.setAttribute('aria-pressed', 'true');
      });
    });

    function paintRange(input) {
      const min = +input.min || 0;
      const max = +input.max || 100;
      const pct = ((+input.value - min) / (max - min)) * 100;
      input.style.setProperty('--p', pct + '%');
    }

    [countRange, speedRange, hueRange].forEach(paintRange);

    /* 小屏幕上粒子密度需要低一些，否则连线会糊成一团 */
    if (window.innerWidth < 640) {
      sim.count = 72;
      countRange.value = '72';
      countOut.textContent = '72';
      paintRange(countRange);
    }

    countRange.addEventListener('input', () => {
      sim.count = +countRange.value;
      countOut.textContent = countRange.value;
      paintRange(countRange);
      refreshParticles();
    });

    speedRange.addEventListener('input', () => {
      sim.speed = +speedRange.value;
      speedOut.textContent = (+speedRange.value).toFixed(1);
      paintRange(speedRange);
    });

    hueRange.addEventListener('input', () => {
      sim.hue = +hueRange.value;
      hueOut.textContent = hueRange.value;
      paintRange(hueRange);
    });

    pauseBtn.addEventListener('click', () => {
      sim.paused = !sim.paused;
      pauseBtn.textContent = sim.paused ? '继续' : '暂停';
      pauseBtn.setAttribute('aria-pressed', String(sim.paused));
    });

    resetBtn.addEventListener('click', () => {
      refreshParticles();
      sim.paused = false;
      pauseBtn.textContent = '暂停';
      pauseBtn.setAttribute('aria-pressed', 'false');
      stageHint.classList.remove('hide');
    });

    /* ==========================================================
       光标辉光
       ========================================================== */
    const glow = document.querySelector('.cursor-glow');
    const dot = document.querySelector('.cursor-dot');
    let mx = window.innerWidth / 2, my = window.innerHeight / 2;
    let gx = mx, gy = my;

    window.addEventListener('pointermove', (e) => {
      if (e.pointerType !== 'mouse') return;
      document.body.classList.add('has-pointer');
      mx = e.clientX;
      my = e.clientY;
      pointer.tx = e.clientX / window.innerWidth;
      pointer.ty = e.clientY / window.innerHeight;
      dot.style.transform = `translate3d(${mx}px, ${my}px, 0)`;
    }, { passive: true });

    (function follow() {
      gx = lerp(gx, mx, 0.11);
      gy = lerp(gy, my, 0.11);
      glow.style.transform = `translate3d(${gx}px, ${gy}px, 0)`;
      requestAnimationFrame(follow);
    })();

    /* ==========================================================
       入场动画
       ========================================================== */
    const revealEls = document.querySelectorAll('.reveal, .card');
    if ('IntersectionObserver' in window) {
      const io = new IntersectionObserver(
        (entries) => {
          entries.forEach((entry) => {
            if (!entry.isIntersecting) return;
            const el = entry.target;
            const delay = reduceMotion ? 0 : (+el.dataset.i || 0) * 80;
            setTimeout(() => el.classList.add('in'), delay);
            io.unobserve(el);
          });
        },
        { threshold: 0.12, rootMargin: '0px 0px -6% 0px' }
      );
      revealEls.forEach((el) => io.observe(el));
    } else {
      revealEls.forEach((el) => el.classList.add('in'));
    }

    /* ==========================================================
       数字滚动
       ========================================================== */
    function countUp(el) {
      const target = +el.dataset.count;
      if (!target) { el.textContent = '0'; return; }
      const duration = 1300;
      const start = performance.now();
      function step(now) {
        const p = clamp((now - start) / duration, 0, 1);
        const eased = 1 - Math.pow(1 - p, 3);
        el.textContent = Math.round(target * eased);
        if (p < 1) requestAnimationFrame(step);
      }
      requestAnimationFrame(step);
    }

    const counters = document.querySelectorAll('[data-count]');
    if ('IntersectionObserver' in window) {
      const co = new IntersectionObserver((entries) => {
        entries.forEach((e) => {
          if (!e.isIntersecting) return;
          countUp(e.target);
          co.unobserve(e.target);
        });
      }, { threshold: 0.4 });
      counters.forEach((el) => co.observe(el));
    } else {
      counters.forEach(countUp);
    }

    /* ==========================================================
       卡片 3D 倾斜
       ========================================================== */
    document.querySelectorAll('[data-tilt]').forEach((card) => {
      card.addEventListener('pointermove', (e) => {
        if (e.pointerType !== 'mouse') return;
        const r = card.getBoundingClientRect();
        const px = (e.clientX - r.left) / r.width;
        const py = (e.clientY - r.top) / r.height;
        card.style.setProperty('--rx', ((0.5 - py) * 9).toFixed(2) + 'deg');
        card.style.setProperty('--ry', ((px - 0.5) * 11).toFixed(2) + 'deg');
        card.style.setProperty('--mx', (px * 100).toFixed(1) + '%');
        card.style.setProperty('--my', (py * 100).toFixed(1) + '%');
        card.classList.add('tilting');
      });
      card.addEventListener('pointerleave', () => {
        card.style.setProperty('--rx', '0deg');
        card.style.setProperty('--ry', '0deg');
        card.classList.remove('tilting');
      });
    });

    /* ==========================================================
       打字机
       ========================================================== */
    const typeText = document.getElementById('typeText');
    const phrases = [
      '正在初始化粒子场…',
      '正在校准光学镜头…',
      '正在加载滚动叙事…',
      '零依赖，直接打开即可运行'
    ];
    if (reduceMotion) {
      typeText.textContent = phrases[0];
    } else {
      let pi = 0, ci = 0, deleting = false;
      (function type() {
        const word = phrases[pi];
        ci += deleting ? -1 : 1;
        typeText.textContent = word.slice(0, ci);
        let wait = deleting ? 34 : 78;
        if (!deleting && ci >= word.length) {
          wait = 1700;
          deleting = true;
        } else if (deleting && ci <= 0) {
          deleting = false;
          pi = (pi + 1) % phrases.length;
          wait = 420;
        }
        setTimeout(type, wait);
      })();
    }

    /* ==========================================================
       滚动进度 + 导航毛玻璃
       ========================================================== */
    const progressBar = document.querySelector('.scroll-progress i');
    const nav = document.getElementById('nav');
    let scrollQueued = false;

    function onScroll() {
      if (scrollQueued) return;
      scrollQueued = true;
      requestAnimationFrame(() => {
        const max = document.documentElement.scrollHeight - window.innerHeight;
        const ratio = max > 0 ? clamp(window.scrollY / max, 0, 1) : 0;
        progressBar.style.transform = `scaleX(${ratio})`;
        nav.classList.toggle('scrolled', window.scrollY > 20);
        scrollQueued = false;
      });
    }
    window.addEventListener('scroll', onScroll, { passive: true });
    onScroll();

    /* ==========================================================
       按钮涟漪 + 复制
       ========================================================== */
    document.querySelectorAll('.btn').forEach((btn) => {
      btn.addEventListener('click', (e) => {
        if (reduceMotion) return;
        const r = btn.getBoundingClientRect();
        const span = document.createElement('span');
        span.className = 'ripple';
        span.style.left = (e.clientX - r.left) + 'px';
        span.style.top = (e.clientY - r.top) + 'px';
        btn.appendChild(span);
        setTimeout(() => span.remove(), 700);
      });
    });

    const copyBtn = document.getElementById('copyBtn');
    const codeBlock = document.getElementById('codeBlock');
    const copyLabel = copyBtn.textContent;

    copyBtn.addEventListener('click', async () => {
      const text = codeBlock.innerText;
      try {
        if (!navigator.clipboard) throw new Error('clipboard unavailable');
        await navigator.clipboard.writeText(text);
      } catch (err) {
        const ta = document.createElement('textarea');
        ta.value = text;
        ta.setAttribute('readonly', '');
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.select();
        try { document.execCommand('copy'); } catch (e) {}
        ta.remove();
      }
      copyBtn.textContent = '已复制 ✓';
      setTimeout(() => { copyBtn.textContent = copyLabel; }, 1600);
    });

    /* ==========================================================
       尺寸变化
       ========================================================== */
    let resizeTimer = 0;
    function handleResize() {
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        bg.resize();
        initStars();
        pc.resize();
        refreshParticles();
        readPalette();
        /* 暂停状态下也要重绘一帧，避免画布被清空 */
        if (sim.paused) updateParticle(0.001);
      }, 140);
    }
    window.addEventListener('resize', handleResize);
    window.addEventListener('orientationchange', handleResize);

    /* 主题变化时重新读取配色 */
    new MutationObserver(readPalette).observe(root, {
      attributes: true,
      attributeFilter: ['data-theme']
    });

    /* ==========================================================
       启动
       ========================================================== */
    bg.resize();
    initStars();
    initMeteors();
    readPalette();
    pc.resize();
    refreshParticles();

    (function bgLoop() {
      if (document.visibilityState === 'visible') {
        drawBackground(performance.now());
      }
      requestAnimationFrame(bgLoop);
    })();

    requestAnimationFrame(loop);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot, { once: true });
  } else {
    boot();
  }
})();
