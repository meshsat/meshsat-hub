/** @type {import('tailwindcss').Config} */

// Brand tokens are CSS variables (see src/style.css) holding "r g b" triplets so
// Tailwind opacity modifiers (bg-brand-primary/15) keep working. Dark is the
// default; `html:not(.dark)` switches to the light set (theme store toggle).
const v = (name) => `rgb(var(--ms-${name}) / <alpha-value>)`

export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{vue,js}'],
  theme: {
    extend: {
      fontFamily: {
        sans: ['IBM Plex Sans', '-apple-system', 'BlinkMacSystemFont', 'Segoe UI', 'Roboto', 'sans-serif'],
        mono: ['IBM Plex Mono', 'ui-monospace', 'SFMono-Regular', 'monospace'],
        display: ['IBM Plex Mono', 'ui-monospace', 'SFMono-Regular', 'monospace'],
      },
      borderRadius: {
        sm: '3px',
        DEFAULT: '5px',
        md: '5px',
        lg: '8px',
        xl: '12px',
      },
      colors: {
        // MeshSat brand (meshsat-website/site/assets/css/main.css token set;
        // Space Black #040406, Signal Orange #F96118, Off White #F7F7F4)
        ms: {
          bg: v('bg'),
          bg2: v('bg2'),
          card: v('card'),
          text: v('text'),
          muted: v('muted'),
          primary: v('primary'),
          'primary-hover': v('primary-hover'),
          'on-primary': v('on-primary'),
          accent: v('accent'),
          border: v('border'),
          'border-light': v('border-light'),
          success: v('success'),
          warning: v('warning'),
          error: v('error'),
        },
        // Legacy names kept as aliases of the brand tokens so existing
        // `brand-*` / `tactical-*` classes re-theme without edits.
        brand: {
          primary: v('primary'),
          accent: v('primary-hover'),
          dark: v('bg'),
          surface: v('card'),
          text: v('text'),
        },
        // Neutral scale: the views were written dark-first with hard-coded
        // gray-*; mapping the scale onto the brand tokens re-themes every
        // surface, border and text tier in both themes without touching them.
        //   900 page bg, 800 well (inputs, hover), 700 border, 600 border-light,
        //   500 muted text, 400 secondary text, 300 body text, 200/100/50 text.
        gray: {
          50: v('text'),
          100: v('text'),
          200: v('text'),
          300: v('text2'),
          400: v('muted2'),
          500: v('muted'),
          600: v('border-light'),
          700: v('border'),
          800: v('well'),
          900: v('bg'),
        },
        // Meaning colours used as text (status, transport, subsystem): the
        // dark values are Tailwind's 400 shades, the light values the 700
        // shades so they stay AA on white.
        green: { 400: v('green') },
        yellow: { 400: v('yellow') },
        sky: { 400: v('sky') },
        blue: { 400: v('blue') },
        purple: { 400: v('purple') },
        cyan: { 400: v('cyan') },
        orange: { 400: v('orange') },
        pink: { 400: v('pink') },
        // Transport badge colors (consistent across Bridge, Hub, Android)
        transport: {
          mesh: v('cyan'),
          iridium: v('purple'),
          cellular: v('orange'),
          sms: v('green'),
        },
        // Tactical UI palette: surfaces follow the brand tokens, the
        // per-subsystem colours encode meaning (theme-aware for contrast).
        tactical: {
          bg: v('bg'),
          surface: v('card'),
          border: v('border'),
          iridium: v('purple'),
          lora: v('cyan'),
          gps: v('indigo'),
          sos: v('error'),
          power: v('success'),
        },
      },
    },
  },
  plugins: [],
}
