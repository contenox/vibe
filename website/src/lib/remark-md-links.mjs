import { visit } from 'unist-util-visit';
import path from 'node:path';

// Heavy media (demo gifs, screenshots, diagrams) lives on S3, not in
// public/. Docs markdown keeps referencing it root-relatively (e.g.
// /hero.gif) — this map rewrites those references at build time. Add a
// filename here when a new asset is uploaded to the bucket.
const S3_MEDIA_BASE =
  'https://contenox-website-assets-573643652148.s3.amazonaws.com/media/';
const S3_MEDIA = new Set([
  'lab-mvp-selfreview.jpg',
  'lab-bob-dashboard.png',
  'lab-bob-connectors.png',
  'lab-bob-search.png',
  'lab-bob-beam.png',
  'lab-bob-files.png',
  'lab-bob-members.png',
  'lab-site-home.png',
  'lab-site-bob-signup.png',
  'lab-site-admin-login.png',
  'lab-admin-bob-tenants.png',
  'lab-admin-bob-tenants2.png',
  'lab-admin-bob-worker-pools.png',
  'lab-admin-bob-apps.png',
  'lab-minio-console.png',
  'lab-mailpit-inbox.png',
  'hero.gif',
  'install.gif',
  'quickstart.gif',
  'chain-blocked.gif',
  'hitl-approve.gif',
  'agent-check.gif',
  'agent-permission-card.png',
  'agent-picker.png',
  'agent-slash-menu.png',
  'chain_flow_diagram.png',
  'hooks_architecture.png',
  'aionui-custom-agent.png',
  // R&D section (docs/rnd/**). Asset log lives outside this repo.
  'beam-demo.webm',
  'chain-runner-demo.webm',
  'backend-manager-demo.mp4',
  'beam-video-cover.png',
  'beam-login.png',
  'beam-new-chat.png',
  'lab-beam-desktop-shell.png',
  'lab-beam-session.png',
  'modeld-console.png',
  'vscode-extension-icon.png',
  'lab-blueprints-ticket-triage.png',
  'lab-blueprints-ticket-triage-form.png',
  'ui-library-storybook.png',
]);

// Rewrites relative links in content:
//  - `*.md` links become their rendered routes. A file at docs/a/b.md renders
//    at /docs/a/b/ (one extra path segment), so a file-relative link needs one
//    leading `../` to stay correct from the page URL. Path segments are
//    lowercased to match the glob loader's slugs (README.md -> readme/).
//  - other relative links (source files, configs) cannot exist on the static
//    site; they are rewritten to GitHub blob URLs resolved against the source
//    markdown file's repo path.
//  - root-relative images whose filename is in S3_MEDIA point at the bucket.
export default function remarkMdLinks() {
  return (tree, file) => {
    const abs = file?.history?.[0] ?? '';
    const marker = `${path.sep}docs${path.sep}`;
    const idx = abs.lastIndexOf(marker);
    const repoDir = idx === -1 ? null : path.posix.dirname('docs/' + abs.slice(idx + marker.length).split(path.sep).join('/'));

    visit(tree, 'image', (node) => {
      const url = node.url ?? '';
      if (url.startsWith('/') && S3_MEDIA.has(url.slice(1))) {
        node.url = S3_MEDIA_BASE + url.slice(1);
      }
    });

    // Raw HTML (e.g. a <video src="/x.webm" poster="/y.png"> demo embed) isn't
    // a markdown image node, so its root-relative attributes need their own pass.
    visit(tree, 'html', (node) => {
      node.value = node.value.replace(/((?:src|poster)=")\/([^"/][^"]*)"/g, (match, prefix, filename) =>
        S3_MEDIA.has(filename) ? `${prefix}${S3_MEDIA_BASE}${filename}"` : match
      );
    });

    visit(tree, 'link', (node) => {
      const url = node.url ?? '';
      if (/^(https?:)?\/\//.test(url) || url.startsWith('/') || url.startsWith('#') || url.startsWith('mailto:')) return;
      if (/\.md(#|$)/.test(url)) {
        const [p, anchor] = url.split('#');
        node.url = '../' + p.toLowerCase().replace(/\.md$/, '/') + (anchor ? `#${anchor}` : '');
        return;
      }
      if (repoDir) {
        const resolved = path.posix.normalize(path.posix.join(repoDir, url));
        if (!resolved.startsWith('..')) {
          node.url = `https://github.com/contenox/contenox/blob/main/${resolved}`;
        } else {
          // Climbs out of docs/ — a repo source path like ../../runtime/foo.go.
          const fromRepoRoot = path.posix.normalize(path.posix.join(repoDir.replace(/^docs/, ''), url)).replace(/^\/+/, '');
          node.url = `https://github.com/contenox/contenox/blob/main/${fromRepoRoot}`;
        }
      }
    });
  };
}
