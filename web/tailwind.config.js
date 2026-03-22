/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{vue,js}'],
  theme: {
    extend: {
      fontFamily: {
        mono: ['JetBrains Mono', 'ui-monospace', 'SFMono-Regular', 'monospace'],
        display: ['Oxanium', 'system-ui', 'sans-serif'],
      },
      colors: {
        // MeshSat brand palette (matches meshsat-android Color.kt + Bridge)
        brand: {
          primary: '#0D9488',   // teal-600
          accent: '#14B8A6',    // teal-500
          dark: '#111827',      // gray-900
          surface: '#1F2937',   // gray-800
          text: '#E5E7EB',      // gray-200
        },
        // Transport badge colors (consistent across Bridge, Hub, Android)
        transport: {
          mesh: '#06B6D4',      // cyan-500
          iridium: '#A855F7',   // purple-500
          cellular: '#F97316',  // orange-500
          sms: '#22C55E',       // green-500
        },
        // Tactical UI palette (matches Bridge)
        tactical: {
          bg: '#111827',        // gray-900
          surface: '#1F2937',   // gray-800
          border: '#374151',    // gray-700
          iridium: '#A855F7',   // purple-500
          lora: '#06B6D4',      // cyan-500
          gps: '#818cf8',       // indigo-500
          sos: '#ef4444',       // red-500
          power: '#10b981',     // emerald-600
        },
      },
    },
  },
  plugins: [],
}
