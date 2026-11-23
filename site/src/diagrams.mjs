// The two diagrams of the peer network, drawn as SVG strings from the words of one language.
// They only use classes (bg, lane, card, line, ...): the landing styles them with the site tokens, and
// docs/guide/img/*.svg embeds the same classes with fixed colours (see scripts/build-diagrams.mjs).

const esc = (text) => text.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
const text = (x, y, value, cls, anchor = 'start') =>
	`<text x="${x}" y="${y}" class="${cls}" text-anchor="${anchor}">${esc(value)}</text>`;
const rect = (x, y, w, h, cls) => `<rect x="${x}" y="${y}" width="${w}" height="${h}" class="${cls}"/>`;
const path = (d, cls, marker) => `<path d="${d}" class="${cls}"${marker ? ` marker-end="url(#${marker})"` : ''}/>`;

const defs = (id) =>
	`<defs><marker id="${id}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto"><path d="M0,1 L9,5 L0,9 z" class="head"/></marker></defs>`;

function svg(id, width, height, body, label, sizeAttributes = false) {
	const size = sizeAttributes ? ` width="${width}" height="${height}"` : '';
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${width} ${height}"${size} role="img" aria-label="${esc(label)}">${defs(id)}${rect(0, 0, width, height, 'bg')}${body}</svg>`;
}

const PEERS = ['laptop', 'mac-mini', 'omarch01'];

function peerCard(x, y, name, words, withJob) {
	const parts = [rect(x, y, 240, 130, 'card'), text(x + 16, y + 30, name, 'mono strong'), text(x + 16, y + 54, 'cflk', 'mono accent')];
	if (withJob) {
		parts.push(rect(x + 16, y + 68, 208, 46, 'box'), text(x + 28, y + 88, words.job, 'strong'), text(x + 28, y + 106, words.jobSub, 'muted small'));
	}
	return parts.join('');
}

// words: { link, tailnet, job, jobSub, note }
export function networkWide(words, label, id = 'net-wide', sizeAttributes = false) {
	const cards = [60, 380, 700].map((x, i) => peerCard(x, 110, PEERS[i], words, i === 2)).join('');
	const links = [
		path('M300,175 H380', 'line'),
		path('M620,175 H700', 'line'),
		path('M180,110 V80 H820 V110', 'line'),
		text(340, 166, words.link, 'mono small muted', 'middle'),
		text(660, 166, words.link, 'mono small muted', 'middle'),
		text(500, 72, words.link, 'mono small muted', 'middle'),
	].join('');
	const body = [
		rect(1, 30, 998, 296, 'lane dash'),
		text(16, 54, words.tailnet, 'mono small muted'),
		links,
		cards,
		text(500, 296, words.note, 'strong', 'middle'),
	].join('');
	return svg(id, 1000, 340, body, label, sizeAttributes);
}

export function networkNarrow(words, label, id = 'net-narrow', sizeAttributes = false) {
	const ys = [60, 246, 432];
	const cards = ys.map((y, i) => peerCard(40, y, PEERS[i], words, i === 2)).join('');
	const links = [
		path('M170,190 V246', 'line'),
		path('M170,376 V432', 'line'),
		path('M40,125 H20 V497 H40', 'line'),
		text(182, 223, words.link, 'mono small muted'),
		`<text transform="translate(12 311) rotate(-90)" class="mono small muted" text-anchor="middle">${esc(words.link)}</text>`,
		text(182, 409, words.link, 'mono small muted'),
	].join('');
	const body = [
		rect(1, 1, 338, 608, 'lane dash'),
		text(16, 30, words.tailnet, 'mono small muted'),
		cards,
		links,
		text(170, 594, words.note, 'strong small', 'middle'),
	].join('');
	return svg(id, 340, 610, body, label, sizeAttributes);
}

function stepBox(x, y, w, n, step) {
	return [
		rect(x, y, w, 84, 'box'),
		text(x + 14, y + 26, `${n}  ${step.title}`, 'strong'),
		text(x + 14, y + 48, step.text, 'small'),
		step.code ? text(x + 14, y + 68, step.code, 'mono accent code') : '',
	].join('');
}

// words: { origin, destination, move, back, backCommand, backText, steps: [{ title, text, code? } x5] }
export function moveWide(words, label, id = 'move-wide', sizeAttributes = false) {
	const colX = (i) => 24 + i * 195;
	const [s1, s2, s3, s4, s5] = words.steps;
	const body = [
		rect(1, 34, 998, 140, 'lane'),
		text(16, 56, words.origin, 'mono small muted'),
		rect(1, 214, 998, 140, 'lane'),
		text(16, 236, words.destination, 'mono small muted'),
		text(16, 20, words.move, 'mono accent'),
		stepBox(colX(0), 78, 165, 1, s1),
		stepBox(colX(1), 78, 165, 2, s2),
		stepBox(colX(2), 78, 165, 3, s3),
		stepBox(colX(3), 258, 165, 4, s4),
		stepBox(colX(4), 258, 165, 5, s5),
		path('M189,120 H219', 'line', id),
		path('M384,120 H414', 'line', id),
		path('M301,162 V290 H609', 'line', id),
		path('M496,162 V306 H609', 'line dash', id),
		path('M774,300 H804', 'line', id),
		text(16, 386, words.back, 'mono small muted'),
		rect(24, 398, 80, 40, 'box'),
		text(64, 423, 'B', 'strong', 'middle'),
		path('M104,418 H284', 'line accent-line', id),
		text(194, 410, words.backCommand, 'mono accent code', 'middle'),
		rect(284, 398, 80, 40, 'box'),
		text(324, 423, 'A', 'strong', 'middle'),
		text(384, 423, words.backText, ''),
	].join('');
	return svg(id, 1000, 456, body, label, sizeAttributes);
}

export function moveNarrow(words, label, id = 'move-narrow', sizeAttributes = false) {
	const body = [
		text(16, 14, words.move, 'mono accent code'),
		rect(1, 28, 338, 340, 'lane'),
		text(16, 50, words.origin, 'mono small muted'),
		rect(1, 388, 338, 240, 'lane'),
		text(16, 410, words.destination, 'mono small muted'),
	];
	const ys = [62, 170, 278, 426, 534];
	words.steps.forEach((step, i) => body.push(stepBox(20, ys[i], 300, i + 1, step)));
	// The arrow into step 4 crosses from A to B, the one place the Job changes machine.
	[[146, 170, 'line'], [254, 278, 'line'], [362, 426, 'line dash'], [510, 534, 'line']].forEach(([from, to, cls]) =>
		body.push(path(`M170,${from} V${to}`, cls, id)),
	);
	body.push(
		text(16, 650, words.back, 'mono small muted'),
		rect(20, 662, 80, 40, 'box'),
		text(60, 687, 'B', 'strong', 'middle'),
		path('M100,682 H260', 'line accent-line', id),
		text(180, 674, words.backCommand, 'mono accent code', 'middle'),
		rect(260, 662, 60, 40, 'box'),
		text(290, 687, 'A', 'strong', 'middle'),
		text(20, 732, words.backText, ''),
	);
	return svg(id, 340, 750, body.join(''), label, sizeAttributes);
}
// The look of a standalone file (an <img> cannot see the page tokens): the same classes, fixed colours.
export const STANDALONE_STYLE = `
.bg{fill:#f6f7f9}.lane{fill:none;stroke:#d5d9e0}.card,.box{fill:#fcfcfd;stroke:#b7bdc9}.line{fill:none;stroke:#8d94a3;stroke-width:1.5}
.accent-line{stroke:#0a6fd1}.dash{stroke-dasharray:5 4}.head{fill:#8d94a3}
text{font:13px 'IBM Plex Sans',system-ui,-apple-system,'Segoe UI',sans-serif;fill:#3b4252}
.strong{font-weight:600;fill:#12151c}.muted{fill:#636b7a}.accent{fill:#0a6fd1}.small{font-size:12px}
.mono{font-family:'IBM Plex Mono',ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.code{font-size:11px}
@media (prefers-color-scheme:dark){.bg{fill:#0f1218}.lane{stroke:#262d3b}.card,.box{fill:#161b24;stroke:#363f52}.line{stroke:#6e7688}
.accent-line{stroke:#1a9bff}.head{fill:#6e7688}text{fill:#b4bac8}.strong{fill:#f4f6f9}.muted{fill:#8b93a5}.accent{fill:#1a9bff}}`;

export function standalone(markup) {
	return markup.replace('<defs>', `<style>${STANDALONE_STYLE}</style><defs>`);
}
