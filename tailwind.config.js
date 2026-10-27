/** Prebuilt design tokens for index.html. Build: npm run build:css (see below)
 *  npx tailwindcss@3 -c tailwind.config.js -i input.css -o styles.css --minify
 *  Colours are rgb(var(--c-*)) so the same classes drive both light + dark. */
module.exports = {
  darkMode: 'class',
  content: ['./index.html'],
  theme: { extend: { colors: {
    canvas:'rgb(var(--c-base) / <alpha-value>)',
    surface:'rgb(var(--c-surface) / <alpha-value>)',
    panel:'rgb(var(--c-panel) / <alpha-value>)',
    panel2:'rgb(var(--c-panel2) / <alpha-value>)',
    line:'rgb(var(--c-line) / <alpha-value>)',
    line2:'rgb(var(--c-line2) / <alpha-value>)',
    ink:'rgb(var(--c-ink) / <alpha-value>)',
    muted:'rgb(var(--c-muted) / <alpha-value>)',
    faint:'rgb(var(--c-faint) / <alpha-value>)',
    accent:'rgb(var(--c-accent) / <alpha-value>)',
    accent2:'rgb(var(--c-accent2) / <alpha-value>)',
    btn:'rgb(var(--c-btn) / <alpha-value>)',
    btn2:'rgb(var(--c-btn2) / <alpha-value>)',
    btnink:'rgb(var(--c-btnink) / <alpha-value>)',
  }}}
}
