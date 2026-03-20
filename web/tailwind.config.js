/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{vue,js}'],
  theme: {
    extend: {
      colors: {
        // MeshSat brand palette (matches meshsat-android Color.kt)
        brand: {
          primary: '#0D9488',   // teal-600
          accent: '#14B8A6',    // teal-500
          dark: '#111827',      // gray-900
          surface: '#1F2937',   // gray-800
          text: '#E5E7EB',      // gray-200
        },
        // Transport badge colors (matches meshsat-android)
        transport: {
          mesh: '#06B6D4',      // cyan-500
          iridium: '#A855F7',   // purple-500
          cellular: '#F97316',  // orange-500
          sms: '#22C55E',       // green-500
        },
      },
    },
  },
  plugins: [],
}
