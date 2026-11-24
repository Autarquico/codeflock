// Writes the guide's copies of the diagrams (docs/guide/img/*.svg) from the same drawing as the landing.
// Run it after changing src/diagrams.mjs or the English words: node scripts/build-diagrams.mjs
import { mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { diagrams } from '../src/i18n/landing.mjs';
import { moveNarrow, networkNarrow, standalone } from '../src/diagrams.mjs';

const out = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', 'docs', 'guide', 'img');
mkdirSync(out, { recursive: true });
const { network, move } = diagrams.en;
writeFileSync(path.join(out, 'network.svg'), `${standalone(networkNarrow(network, network.alt, 'net', true))}\n`);
writeFileSync(path.join(out, 'move-a-job.svg'), `${standalone(moveNarrow(move, move.alt, 'move', true))}\n`);
