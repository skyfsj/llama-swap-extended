// Unit Converter - one tool, unit_convert.
//
// Everything is offline: a factor table in this file, no network, no key. The
// interesting part is not the multiplication but the refusals - a converter that
// silently answers "5" to "5 cm in kg" is worse than one that says the question
// has no answer, and a table that quietly guesses what "mb" means will be wrong
// half the time. So each family carries its base unit, every unit carries its
// factor to that base, and anything ambiguous is reported as ambiguous.
//
// Temperature is affine (K = C + 273.15, F = C * 9/5 + 32), not a ratio, so it is
// handled by formulas rather than factors. Torque stays separate from energy
// even though both are built from newtons and metres: N·m and J measure
// different things, and converting one to the other is a category error.

// family: { name, base, units: { alias: [factor, canonical] } }
//
// Factors are exact where the definition is exact (an inch is exactly 25.4 mm,
// a US gallon exactly 231 in^3) and to the published precision otherwise. The
// canonical spelling is what the answer prints, so the caller sees the unit it
// asked for rather than an alias echoed back.
const FAMILIES = {
  length: {
    name: "length",
    base: "m",
    units: {
      m: [1, "m"], metre: [1, "m"], meter: [1, "m"], metres: [1, "m"], meters: [1, "m"],
      km: [1000, "km"], kilometre: [1000, "km"], kilometer: [1000, "km"], kilometres: [1000, "km"], kilometers: [1000, "km"],
      cm: [0.01, "cm"], centimetre: [0.01, "cm"], centimeter: [0.01, "cm"], centimetres: [0.01, "cm"], centimeters: [0.01, "cm"],
      mm: [0.001, "mm"], millimetre: [0.001, "mm"], millimeter: [0.001, "mm"], millimetres: [0.001, "mm"], millimeters: [0.001, "mm"],
      um: [1e-6, "µm"], micrometre: [1e-6, "µm"], micrometer: [1e-6, "µm"], micron: [1e-6, "µm"], microns: [1e-6, "µm"],
      nm: [1e-9, "nm"], nanometre: [1e-9, "nm"], nanometer: [1e-9, "nm"], nanometres: [1e-9, "nm"], nanometers: [1e-9, "nm"],
      mi: [1609.344, "mi"], mile: [1609.344, "mi"], miles: [1609.344, "mi"],
      yd: [0.9144, "yd"], yard: [0.9144, "yd"], yards: [0.9144, "yd"],
      ft: [0.3048, "ft"], foot: [0.3048, "ft"], feet: [0.3048, "ft"], "'": [0.3048, "ft"],
      in: [0.0254, "in"], inch: [0.0254, "in"], inches: [0.0254, "in"], '"': [0.0254, "in"],
      nmi: [1852, "nmi"], "nautical-mile": [1852, "nmi"], nauticalmile: [1852, "nmi"], nauticalmiles: [1852, "nmi"],
      ly: [9.4607304725808e15, "ly"], "light-year": [9.4607304725808e15, "ly"], lightyear: [9.4607304725808e15, "ly"], lightyears: [9.4607304725808e15, "ly"],
      au: [1.495978707e11, "au"], "astronomical-unit": [1.495978707e11, "au"],
      pc: [3.0856775814913673e16, "pc"], parsec: [3.0856775814913673e16, "pc"], parsecs: [3.0856775814913673e16, "pc"],
      angstrom: [1e-10, "Å"], å: [1e-10, "Å"], "å": [1e-10, "Å"],
      fathom: [1.8288, "fathom"], fathoms: [1.8288, "fathom"],
      furlong: [201.168, "furlong"], furlongs: [201.168, "furlong"],
    },
  },
  mass: {
    name: "mass",
    base: "kg",
    units: {
      kg: [1, "kg"], kilogram: [1, "kg"], kilograms: [1, "kg"], kilogramme: [1, "kg"], kilogrammes: [1, "kg"],
      g: [0.001, "g"], gram: [0.001, "g"], grams: [0.001, "g"], gramme: [0.001, "g"], grammes: [0.001, "g"],
      mg: [1e-6, "mg"], milligram: [1e-6, "mg"], milligrams: [1e-6, "mg"],
      ug: [1e-9, "µg"], microgram: [1e-9, "µg"], micrograms: [1e-9, "µg"],
      tonne: [1000, "t"], tonnes: [1000, "t"], t: [1000, "t"], metricton: [1000, "t"], metrictons: [1000, "t"],
      lb: [0.45359237, "lb"], lbs: [0.45359237, "lb"], pound: [0.45359237, "lb"], pounds: [0.45359237, "lb"],
      oz: [0.028349523125, "oz"], ounce: [0.028349523125, "oz"], ounces: [0.028349523125, "oz"],
      stone: [6.35029318, "st"], stones: [6.35029318, "st"], st: [6.35029318, "st"],
      ton: [907.18474, "ton"], tons: [907.18474, "ton"], shortton: [907.18474, "ton"], shorttons: [907.18474, "ton"],
      longton: [1016.0469088, "long ton"], longtons: [1016.0469088, "long ton"],
      grain: [6.479891e-5, "grain"], grains: [6.479891e-5, "grain"],
      carat: [0.0002, "ct"], carats: [0.0002, "ct"], ct: [0.0002, "ct"],
      dalton: [1.66053906660e-27, "Da"], da: [1.66053906660e-27, "Da"], amu: [1.66053906660e-27, "Da"],
    },
  },
  area: {
    name: "area",
    base: "m²",
    units: {
      "m2": [1, "m²"], "m²": [1, "m²"], sqm: [1, "m²"], "square-meter": [1, "m²"], "square-metre": [1, "m²"],
      "square-meters": [1, "m²"], "square-metres": [1, "m²"], squaremeter: [1, "m²"], squaremetre: [1, "m²"],
      "cm2": [1e-4, "cm²"], "cm²": [1e-4, "cm²"], "square-centimeter": [1e-4, "cm²"], "square-centimetre": [1e-4, "cm²"],
      "mm2": [1e-6, "mm²"], "mm²": [1e-6, "mm²"],
      "km2": [1e6, "km²"], "km²": [1e6, "km²"], "square-kilometer": [1e6, "km²"], "square-kilometre": [1e6, "km²"],
      ha: [10000, "ha"], hectare: [10000, "ha"], hectares: [10000, "ha"],
      acre: [4046.8564224, "acre"], acres: [4046.8564224, "acre"],
      "ft2": [0.09290304, "ft²"], "ft²": [0.09290304, "ft²"], sqft: [0.09290304, "ft²"], "square-foot": [0.09290304, "ft²"], "square-feet": [0.09290304, "ft²"],
      "in2": [0.00064516, "in²"], "in²": [0.00064516, "in²"], sqin: [0.00064516, "in²"], "square-inch": [0.00064516, "in²"], "square-inches": [0.00064516, "in²"],
      "yd2": [0.83612736, "yd²"], "yd²": [0.83612736, "yd²"], sqyd: [0.83612736, "yd²"], "square-yard": [0.83612736, "yd²"],
      "mi2": [2589988.110336, "mi²"], "mi²": [2589988.110336, "mi²"], "square-mile": [2589988.110336, "mi²"], "square-miles": [2589988.110336, "mi²"],
    },
  },
  volume: {
    name: "volume",
    base: "L",
    units: {
      l: [1, "L"], L: [1, "L"], litre: [1, "L"], liter: [1, "L"], litres: [1, "L"], liters: [1, "L"],
      ml: [0.001, "mL"], millilitre: [0.001, "mL"], milliliter: [0.001, "mL"], millilitres: [0.001, "mL"], milliliters: [0.001, "mL"], cc: [0.001, "mL"],
      ML: [1000, "ML"], megalitre: [1000, "ML"], megaliter: [1000, "ML"], megalitres: [1000, "ML"], megaliters: [1000, "ML"],
      "m3": [1000, "m³"], "m³": [1000, "m³"], "cubic-meter": [1000, "m³"], "cubic-metre": [1000, "m³"], "cubic-meters": [1000, "m³"], "cubic-metres": [1000, "m³"],
      "cm3": [0.001, "cm³"], "cm³": [0.001, "cm³"], "cubic-centimeter": [0.001, "cm³"], "cubic-centimetre": [0.001, "cm³"],
      gal: [3.785411784, "gal (US)"], gallon: [3.785411784, "gal (US)"], gallons: [3.785411784, "gal (US)"], "us-gal": [3.785411784, "gal (US)"], "gal-us": [3.785411784, "gal (US)"],
      "uk-gal": [4.54609, "gal (UK)"], "imperial-gallon": [4.54609, "gal (UK)"], "gal-uk": [4.54609, "gal (UK)"],
      qt: [0.946352946, "qt (US)"], quart: [0.946352946, "qt (US)"], quarts: [0.946352946, "qt (US)"],
      pt: [0.473176473, "pt (US)"], pint: [0.473176473, "pt (US)"], pints: [0.473176473, "pt (US)"],
      cup: [0.2365882365, "cup (US)"], cups: [0.2365882365, "cup (US)"],
      "floz": [0.0295735295625, "fl oz (US)"], "fl-oz": [0.0295735295625, "fl oz (US)"], fluidounce: [0.0295735295625, "fl oz (US)"], fluidounces: [0.0295735295625, "fl oz (US)"], "us-fl-oz": [0.0295735295625, "fl oz (US)"],
      tbsp: [0.01478676478125, "tbsp"], tablespoon: [0.01478676478125, "tbsp"], tablespoons: [0.01478676478125, "tbsp"],
      tsp: [0.00492892159375, "tsp"], teaspoon: [0.00492892159375, "tsp"], teaspoons: [0.00492892159375, "tsp"],
      "ft3": [28.316846592, "ft³"], "ft³": [28.316846592, "ft³"], "cubic-foot": [28.316846592, "ft³"], "cubic-feet": [28.316846592, "ft³"],
      "in3": [0.016387064, "in³"], "in³": [0.016387064, "in³"], "cubic-inch": [0.016387064, "in³"], "cubic-inches": [0.016387064, "in³"],
      barrel: [158.987294928, "bbl (oil)"], barrels: [158.987294928, "bbl (oil)"], bbl: [158.987294928, "bbl (oil)"],
    },
  },
  time: {
    name: "time",
    base: "s",
    units: {
      s: [1, "s"], sec: [1, "s"], secs: [1, "s"], second: [1, "s"], seconds: [1, "s"],
      ms: [0.001, "ms"], millisecond: [0.001, "ms"], milliseconds: [0.001, "ms"],
      us: [1e-6, "µs"], microsecond: [1e-6, "µs"], microseconds: [1e-6, "µs"],
      ns: [1e-9, "ns"], nanosecond: [1e-9, "ns"], nanoseconds: [1e-9, "ns"],
      as: [1e-18, "as"], attosecond: [1e-18, "as"], attoseconds: [1e-18, "as"],
      min: [60, "min"], mins: [60, "min"], minute: [60, "min"], minutes: [60, "min"],
      h: [3600, "h"], hr: [3600, "h"], hrs: [3600, "h"], hour: [3600, "h"], hours: [3600, "h"],
      d: [86400, "d"], day: [86400, "d"], days: [86400, "d"],
      w: [604800, "wk"], week: [604800, "wk"], weeks: [604800, "wk"], wk: [604800, "wk"],
      mo: [2592000, "mo (30 d)"], month: [2592000, "mo (30 d)"], months: [2592000, "mo (30 d)"],
      y: [31557600, "yr (365.25 d)"], yr: [31557600, "yr (365.25 d)"], year: [31557600, "yr (365.25 d)"], years: [31557600, "yr (365.25 d)"],
      decade: [315576000, "decade"], decades: [315576000, "decade"],
      fortnight: [1209600, "fortnight"], fortnights: [1209600, "fortnight"],
    },
  },
  speed: {
    name: "speed",
    base: "m/s",
    units: {
      "m/s": [1, "m/s"], mps: [1, "m/s"], "metre-per-second": [1, "m/s"], "meter-per-second": [1, "m/s"],
      "km/h": [1 / 3.6, "km/h"], kmh: [1 / 3.6, "km/h"], kph: [1 / 3.6, "km/h"], "km/hr": [1 / 3.6, "km/h"],
      mph: [0.44704, "mph"], "mi/h": [0.44704, "mph"],
      kn: [0.5144444444444445, "kn"], knot: [0.5144444444444445, "kn"], knots: [0.5144444444444445, "kn"],
      "ft/s": [0.3048, "ft/s"], fps: [0.3048, "ft/s"],
      c: [299792458, "c"],
      mach: [340.29, "Ma"], ma: [340.29, "Ma"],
    },
  },
  pressure: {
    name: "pressure",
    base: "Pa",
    units: {
      pa: [1, "Pa"], pascal: [1, "Pa"], pascals: [1, "Pa"],
      kpa: [1000, "kPa"], kilopascal: [1000, "kPa"], kilopascals: [1000, "kPa"],
      mpa: [1e6, "MPa"], megapascal: [1e6, "MPa"], megapascals: [1e6, "MPa"],
      bar: [100000, "bar"], bars: [100000, "bar"],
      mbar: [100, "mbar"], millibar: [100, "mbar"], millibars: [100, "mbar"], hpa: [100, "mbar"], hectopascal: [100, "mbar"], hectopascals: [100, "mbar"],
      psi: [6894.757293168361, "psi"], "lb/in2": [6894.757293168361, "psi"],
      atm: [101325, "atm"], atmosphere: [101325, "atm"], atmospheres: [101325, "atm"],
      torr: [133.322387415, "Torr"], mmhg: [133.322387415, "mmHg"], "mm-hg": [133.322387415, "mmHg"],
      inhg: [3386.389, "inHg"], "in-hg": [3386.389, "inHg"],
    },
  },
  energy: {
    name: "energy",
    base: "J",
    units: {
      j: [1, "J"], joule: [1, "J"], joules: [1, "J"],
      kj: [1000, "kJ"], kilojoule: [1000, "kJ"], kilojoules: [1000, "kJ"],
      mj: [1e6, "MJ"], megajoule: [1e6, "MJ"], megajoules: [1e6, "MJ"],
      mJ: [0.001, "mJ"], millijoule: [0.001, "mJ"], millijoules: [0.001, "mJ"],
      cal: [4.184, "cal"], calorie: [4.184, "cal"], calories: [4.184, "cal"],
      kcal: [4184, "kcal"], kilocalorie: [4184, "kcal"], kilocalories: [4184, "kcal"], Cal: [4184, "kcal"],
      wh: [3600, "Wh"], "w-h": [3600, "Wh"], watthour: [3600, "Wh"], watthours: [3600, "Wh"], "watt-hour": [3600, "Wh"], "watt-hours": [3600, "Wh"],
      kwh: [3600000, "kWh"], kilowatthour: [3600000, "kWh"], "kilowatt-hour": [3600000, "kWh"], "kilowatt-hours": [3600000, "kWh"],
      ev: [1.602176634e-19, "eV"], electronvolt: [1.602176634e-19, "eV"], electronvolts: [1.602176634e-19, "eV"],
      btu: [1055.05585262, "BTU"], btus: [1055.05585262, "BTU"],
      erg: [1e-7, "erg"], ergs: [1e-7, "erg"],
      therm: [1.05505585262e8, "therm"], therms: [1.05505585262e8, "therm"],
      tnt: [4.184e9, "ton TNT"], "ton-tnt": [4.184e9, "ton TNT"], kiloton: [4.184e12, "kt TNT"],
    },
  },
  power: {
    name: "power",
    base: "W",
    units: {
      w: [1, "W"], watt: [1, "W"], watts: [1, "W"],
      kw: [1000, "kW"], kilowatt: [1000, "kW"], kilowatts: [1000, "kW"],
      mw: [1e6, "MW"], megawatt: [1e6, "MW"], megawatts: [1e6, "MW"],
      // A milliwatt differs from a megawatt only by the case of its prefix, so
      // both spellings have to exist for the exact match to tell them apart.
      mW: [0.001, "mW"], milliwatt: [0.001, "mW"], milliwatts: [0.001, "mW"],
      gw: [1e9, "GW"], gigawatt: [1e9, "GW"], gigawatts: [1e9, "GW"],
      hp: [745.6998715822702, "hp"], horsepower: [745.6998715822702, "hp"],
      "hp-metric": [735.49875, "PS"], ps: [735.49875, "PS"], "metric-horsepower": [735.49875, "PS"],
      "btu/h": [0.2930710701722222, "BTU/h"], "btuh": [0.2930710701722222, "BTU/h"],
      "cal/s": [4.184, "cal/s"],
    },
  },
  angle: {
    name: "angle",
    base: "rad",
    units: {
      rad: [1, "rad"], radian: [1, "rad"], radians: [1, "rad"],
      deg: [Math.PI / 180, "°"], degree: [Math.PI / 180, "°"], degrees: [Math.PI / 180, "°"], "°": [Math.PI / 180, "°"],
      grad: [Math.PI / 200, "grad"], gradian: [Math.PI / 200, "grad"], gradians: [Math.PI / 200, "grad"], gon: [Math.PI / 200, "grad"],
      turn: [2 * Math.PI, "turn"], turns: [2 * Math.PI, "turn"], revolution: [2 * Math.PI, "turn"], revolutions: [2 * Math.PI, "turn"], rev: [2 * Math.PI, "turn"],
      arcmin: [Math.PI / 10800, "′"], arcminute: [Math.PI / 10800, "′"], "′": [Math.PI / 10800, "′"],
      arcsec: [Math.PI / 648000, "″"], arcsecond: [Math.PI / 648000, "″"], "″": [Math.PI / 648000, "″"],
      mrad: [0.001, "mrad"], milliradian: [0.001, "mrad"], milliradians: [0.001, "mrad"],
    },
  },
  frequency: {
    name: "frequency",
    base: "Hz",
    units: {
      hz: [1, "Hz"], hertz: [1, "Hz"],
      khz: [1000, "kHz"], kilohertz: [1000, "kHz"],
      mhz: [1e6, "MHz"], megahertz: [1e6, "MHz"],
      ghz: [1e9, "GHz"], gigahertz: [1e9, "GHz"],
      thz: [1e12, "THz"], terahertz: [1e12, "THz"],
      rpm: [1 / 60, "rpm"],
      bpm: [1 / 60, "bpm"],
    },
  },
  data: {
    name: "data",
    base: "B",
    units: {
      // Decimal (SI) and binary prefixes are separate entries on purpose:
      // "GB" and "GiB" differ by 7%, and a tool that conflates them will be
      // wrong about a disk size.
      b: [1, "B"], byte: [1, "B"], bytes: [1, "B"], octet: [1, "B"], octets: [1, "B"],
      kb: [1000, "kB"], kilobyte: [1000, "kB"], kilobytes: [1000, "kB"],
      mb: [1e6, "MB"], megabyte: [1e6, "MB"], megabytes: [1e6, "MB"],
      gb: [1e9, "GB"], gigabyte: [1e9, "GB"], gigabytes: [1e9, "GB"],
      tb: [1e12, "TB"], terabyte: [1e12, "TB"], terabytes: [1e12, "TB"],
      pb: [1e15, "PB"], petabyte: [1e15, "PB"], petabytes: [1e15, "PB"],
      Kb: [125, "Kbit"], kilobit: [125, "Kbit"], kilobits: [125, "Kbit"],
      Gb: [125000000, "Gbit"], gigabit: [125000000, "Gbit"], gigabits: [125000000, "Gbit"],
      Tb: [125000000000, "Tbit"], terabit: [125000000000, "Tbit"], terabits: [125000000000, "Tbit"],
      Pb: [125000000000000, "Pbit"], petabit: [125000000000000, "Pbit"], petabits: [125000000000000, "Pbit"],
      mB: [0.001, "mB"], millibyte: [0.001, "mB"],
      kib: [1024, "KiB"], kibibyte: [1024, "KiB"], kibibytes: [1024, "KiB"],
      mib: [1048576, "MiB"], mebibyte: [1048576, "MiB"], mebibytes: [1048576, "MiB"],
      gib: [1073741824, "GiB"], gibibyte: [1073741824, "GiB"], gibibytes: [1073741824, "GiB"],
      tib: [1099511627776, "TiB"], tebibyte: [1099511627776, "TiB"], tebibytes: [1099511627776, "TiB"],
      pib: [1125899906842624, "PiB"], pebibyte: [1125899906842624, "PiB"], pebibytes: [1125899906842624, "PiB"],
      bit: [0.125, "bit"], bits: [0.125, "bit"],
      kbit: [125, "kbit"], kilobit: [125, "kbit"], kilobits: [125, "kbit"],
      mbit: [125000, "Mbit"], megabit: [125000, "Mbit"], megabits: [125000, "Mbit"], Mb: [125000, "Mbit"],
      gbit: [125000000, "Gbit"], gigabit: [125000000, "Gbit"], gigabits: [125000000, "Gbit"],
      kibit: [128, "Kibit"], mebibit: [131072, "Mibit"], mibit: [131072, "Mibit"], gibibit: [134217728, "Gibit"], gibit: [134217728, "Gibit"],
      // Throughput, as a family of its own inside data: the byte-per-second and
      // bit-per-second forms differ by a factor of eight and are both written
      // with a slash.
      "b/s": [1, "B/s"], "B/s": [1, "B/s"], "byte/s": [1, "B/s"], "bytes/s": [1, "B/s"],
      // Conventional networking spelling: bps is bits per second, B/s is bytes.
      bps: [0.125, "bit/s"],
      "kb/s": [1000, "kB/s"], "kib/s": [1024, "KiB/s"],
      "mb/s": [1e6, "MB/s"], "mib/s": [1048576, "MiB/s"],
      "gb/s": [1e9, "GB/s"], "gib/s": [1073741824, "GiB/s"],
      "bit/s": [0.125, "bit/s"], "kbit/s": [125, "kbit/s"], "mbit/s": [125000, "Mbit/s"],
      "gbit/s": [125000000, "Gbit/s"], "mbit/s" : [125000, "Mbit/s"],
      mbps: [125000, "Mbit/s"], Mbps: [125000, "Mbit/s"], kbps: [125, "kbit/s"], gbps: [125000000, "Gbit/s"],
    },
  },
  force: {
    name: "force",
    base: "N",
    units: {
      n: [1, "N"], newton: [1, "N"], newtons: [1, "N"],
      kn: [1000, "kN"], kilonewton: [1000, "kN"], kilonewtons: [1000, "kN"],
      mn: [0.001, "mN"], millinewton: [0.001, "mN"], millinewtons: [0.001, "mN"],
      MN: [1e6, "MN"], meganewton: [1e6, "MN"], meganewtons: [1e6, "MN"],
      lbf: [4.4482216152605, "lbf"], poundforce: [4.4482216152605, "lbf"], "pound-force": [4.4482216152605, "lbf"], "lb-force": [4.4482216152605, "lbf"],
      kgf: [9.80665, "kgf"], "kilogram-force": [9.80665, "kgf"], kilopond: [9.80665, "kgf"], kp: [9.80665, "kgf"],
      dyn: [1e-5, "dyn"], dyne: [1e-5, "dyn"], dynes: [1e-5, "dyn"],
      ozf: [0.2780138509537812, "ozf"], ounceforce: [0.2780138509537812, "ozf"],
    },
  },
  torque: {
    name: "torque",
    base: "N·m",
    units: {
      // Torque is not energy: N·m and J have the same dimensions but one is a
      // vector quantity about an axis and the other is work. Keeping them in
      // separate families is what stops "5 N·m in joules" from answering.
      "n*m": [1, "N·m"], "n·m": [1, "N·m"], nm: [1, "N·m"], newtonmeter: [1, "N·m"], newtonmetre: [1, "N·m"], "newton-meter": [1, "N·m"], "newton-metre": [1, "N·m"],
      "kgf*m": [9.80665, "kgf·m"], "kgf·m": [9.80665, "kgf·m"], "kilogram-force-meter": [9.80665, "kgf·m"],
      "lbf*ft": [1.3558179483314004, "lbf·ft"], "lbf·ft": [1.3558179483314004, "lbf·ft"], "lb-ft": [1.3558179483314004, "lbf·ft"], "pound-foot": [1.3558179483314004, "lbf·ft"],
      "lbf*in": [0.1129848290276167, "lbf·in"], "lbf·in": [0.1129848290276167, "lbf·in"], "lb-in": [0.1129848290276167, "lbf·in"],
      "dyn*cm": [1e-7, "dyn·cm"], "dyn·cm": [1e-7, "dyn·cm"],
    },
  },
};

// Temperature is affine, so it does not fit the factor tables: each entry is a
// pair of functions to and from kelvin.
const TEMPERATURE = {
  name: "temperature",
  base: "K",
  units: {
    k: "K", kelvin: "K", kelvins: "K",
    c: "°C", "°c": "°C", celsius: "°C", centigrade: "°C", "degc": "°C", "degree-celsius": "°C",
    f: "°F", "°f": "°F", fahrenheit: "°F", "degf": "°F", "degree-fahrenheit": "°F",
    r: "°R", rankine: "°R", "°r": "°R",
    re: "°Ré", réaumur: "°Ré", reaumur: "°Ré",
  },
  toKelvin: (value, unit) => {
    switch (unit) {
      case "K": return value;
      case "°C": return value + 273.15;
      case "°F": return (value - 32) * (5 / 9) + 273.15;
      case "°R": return value * (5 / 9);
      case "°Ré": return value * (5 / 4) + 273.15;
      default: throw new Error("internal: unknown temperature unit " + unit);
    }
  },
  fromKelvin: (value, unit) => {
    switch (unit) {
      case "K": return value;
      case "°C": return value - 273.15;
      case "°F": return (value - 273.15) * (9 / 5) + 32;
      case "°R": return value * (9 / 5);
      case "°Ré": return (value - 273.15) * (4 / 5);
      default: throw new Error("internal: unknown temperature unit " + unit);
    }
  },
  formula: (unit) => {
    switch (unit) {
      case "K": return "K = °C + 273.15";
      case "°C": return "°C = K - 273.15";
      case "°F": return "°F = °C x 9/5 + 32";
      case "°R": return "°R = °C x 9/5 + 491.67";
      case "°Ré": return "°Ré = °C x 4/5";
      default: return "";
    }
  },
};

// Spellings that name two different things. They are consulted only after the
// exact and folded lookups have both failed, so an entry here has to be a name
// no unit claims: "p" (pascal, pint, parsec) qualifies, while "mb" resolves to
// the megabyte and never reaches this table.
const AMBIGUOUS = {
  mb: ["MB (megabyte, 10^6 B)", "Mb (megabit, 10^6 bit)", "MiB (mebibyte, 2^20 B)"],
  m: ["m (metre, length)", "mi (mile, length)", "min (minute, time)"],
  p: ["Pa (pascal, pressure)", "pt (pint, volume)", "pc (parsec, length)"],
  r: ["R (rankine, temperature)", "rad (radian, angle)", "rpm (revolutions per minute, frequency)"],
  t: ["t (tonne, mass)", "tsp (teaspoon, volume)", "tbsp (tablespoon, volume)"],
};

// Spellings whose only difference is the case of an SI prefix are different
// units, and a folded lookup would answer with the wrong one by a factor of a
// billion. These are matched exactly or refused, never folded.
const PREFIX_CASE_UNITS = {
  mw: ["mW (milliwatt)", "MW (megawatt)"],
  mn: ["mN (millinewton)", "MN (meganewton)"],
  mj: ["mJ (millijoule)", "MJ (megajoule)"],
  ml: ["mL (millilitre)", "ML (megalitre)"],
  mb: ["MB (megabyte, 10^6 B)", "Mb (megabit, 10^6 bit)", "mB (millibyte)"],
  kb: ["kB (kilobyte, 1000 B)", "Kb (kilobit, 1000 bit)"],
  gb: ["GB (gigabyte, 10^9 B)", "Gb (gigabit, 10^9 bit)"],
  tb: ["TB (terabyte, 10^12 B)", "Tb (terabit, 10^12 bit)"],
  pb: ["PB (petabyte, 10^15 B)", "Pb (petabit, 10^15 bit)"],
};

// Units whose meaning is a logarithm; a factor table cannot express them.
const LOGARITHMIC_UNITS = {
  db: "dB", dbm: "dBm", dbi: "dBi", dbw: "dBW", neper: "neper", np: "neper",
};

const FAMILY_ORDER = ["length", "mass", "area", "volume", "temperature", "time", "speed", "pressure", "energy", "power", "angle", "frequency", "data", "force", "torque"];

// The unit index is built once: one entry per accepted spelling, exact case
// first, with a folded copy as the fallback that lets "KILOMETRE" work.
//
// Both the alias and the canonical spelling are indexed, because the tables are
// written in lower case while the canonical names are not: "W" is only findable
// as the canonical watt, and "w" is the alias for a week. That is exactly the
// distinction a folded lookup destroys, which is why exact matching runs first.
const EXACT = new Map();
const FOLDED = new Map();
const FOLDED_CONFLICTS = new Set();

function indexUnit(spelling, entry) {
  if (!EXACT.has(spelling)) {
    EXACT.set(spelling, entry);
  }
  const folded = spelling.toLowerCase();
  const existing = FOLDED.get(folded);
  if (existing === undefined) {
    FOLDED.set(folded, entry);
  } else if (existing.family !== entry.family || existing.canonical !== entry.canonical) {
    FOLDED_CONFLICTS.add(folded);
  }
}

for (const alias of Object.keys(TEMPERATURE.units)) {
  const canonical = TEMPERATURE.units[alias];
  indexUnit(alias, { family: "temperature", canonical: canonical, temperature: true });
  indexUnit(canonical, { family: "temperature", canonical: canonical, temperature: true });
}
for (const familyName of FAMILY_ORDER) {
  const family = FAMILIES[familyName];
  if (!family) {
    continue;
  }
  for (const alias of Object.keys(family.units)) {
    const entry = family.units[alias];
    if (!entry) {
      continue;
    }
    const resolved = { family: familyName, canonical: entry[1], factor: entry[0], base: family.base };
    indexUnit(alias, resolved);
    indexUnit(entry[1], resolved);
  }
}

// resolve finds a unit by name. An exact spelling wins; only an unknown one falls
// back to the folded form, and a spelling that folded to two different units is
// refused rather than guessed at.
function resolve(raw) {
  const text = String(raw === undefined || raw === null ? "" : raw).trim();
  if (text === "") {
    throw new Error("unit_convert: a unit name is required");
  }
  const exact = EXACT.get(text);
  if (exact) {
    return exact;
  }
  const folded = text.toLowerCase();
  // A spelling that only differs from another unit by prefix case is not the
  // same spelling: "mW" is a milliwatt and "MW" a megawatt, a factor of a
  // billion apart. Refusing is the only honest answer when the exact form is
  // unknown.
  if (Object.prototype.hasOwnProperty.call(PREFIX_CASE_UNITS, folded)) {
    // Every name in the advice resolves exactly, so following it works. The
    // table is only reached when the caller's spelling is unknown and its folded
    // form is a prefix-case pair: "MW" must not quietly become a milliwatt.
    const options = PREFIX_CASE_UNITS[folded].filter((entry) => EXACT.has(entry.split(" ")[0]));
    throw new Error("unit_convert: \"" + text + "\" is not a recognised spelling; the case decides the unit - write one of: " +
      (options.length > 0 ? options.join(", ") : PREFIX_CASE_UNITS[folded].join(", ")));
  }
  if (!FOLDED_CONFLICTS.has(folded)) {
    const entry = FOLDED.get(folded);
    if (entry) {
      return entry;
    }
  }
  if (Object.prototype.hasOwnProperty.call(AMBIGUOUS, folded)) {
    throw new Error("unit_convert: \"" + text + "\" is ambiguous - write one of: " + AMBIGUOUS[folded].join(", "));
  }
  if (Object.prototype.hasOwnProperty.call(LOGARITHMIC_UNITS, folded)) {
    throw new Error("unit_convert: " + LOGARITHMIC_UNITS[folded] + " is a logarithmic level, not a ratio, so it cannot be converted by this tool; convert it to the linear unit it references (dBm to mW, dB to a power ratio) outside the tool first");
  }
  throw new Error("unit_convert: unknown unit \"" + text + "\"" + suggestion(text));
}

// suggestion names the closest known units, so a typo comes back with the fix
// instead of a list of everything.
function suggestion(text) {
  const folded = text.toLowerCase();
  const candidates = [];
  for (const familyName of FAMILY_ORDER) {
    const family = FAMILIES[familyName];
    if (!family) {
      continue;
    }
    for (const alias of Object.keys(family.units)) {
      const entry = family.units[alias];
      if (!entry) {
        continue;
      }
      const aliasFolded = alias.toLowerCase();
      if (aliasFolded === folded) {
        continue;
      }
      if (aliasFolded.indexOf(folded) === 0 || folded.indexOf(aliasFolded) === 0) {
        candidates.push(entry[1]);
      }
    }
  }
  for (const alias of Object.keys(TEMPERATURE.units)) {
    if (alias.indexOf(folded) === 0 || folded.indexOf(alias) === 0) {
      candidates.push(TEMPERATURE.units[alias]);
    }
  }
  const unique = [];
  for (const candidate of candidates) {
    if (unique.indexOf(candidate) < 0) {
      unique.push(candidate);
    }
  }
  if (unique.length === 0) {
    return ". Use a full unit name such as kilometre, pound, litre, celsius or MiB, or ask for action:\"families\" to list what is supported.";
  }
  return ". Did you mean " + unique.slice(0, 6).join(", ") + "?";
}

export const settings = {
  precision: {
    type: "number",
    label: "Significant digits",
    hint: "How many significant digits an answer carries (1-15). 6 is enough for everyday work; raise it when a small difference matters.",
    default: 6,
    min: 1,
    max: 15,
    section: "Output",
  },
  verbose: {
    type: "boolean",
    label: "Show the factor",
    hint: "Print the exact factor used and, for temperature, the formula. Useful when the number will be checked by hand.",
    default: false,
    section: "Output",
  },
  maxTargets: {
    type: "number",
    label: "Targets per call",
    hint: "How many units one call may convert to when 'to' is a list.",
    default: 12,
    min: 1,
    max: 50,
    section: "Output",
  },
  showAliases: {
    type: "boolean",
    label: "List aliases",
    hint: "In the family listing, show the accepted spellings for each unit.",
    default: false,
    section: "Output",
  },
};

function number(value, fallback) {
  const parsed = Number(value);
  return isFinite(parsed) ? parsed : fallback;
}

function integer(value, fallback, low, high) {
  const parsed = Math.floor(number(value, fallback));
  if (!isFinite(parsed)) {
    return fallback;
  }
  return Math.max(low, Math.min(high, parsed));
}

// formatNumber rounds to significant digits without an exponent for the sizes a
// person writes, and falls back to an exponent when the number is extreme.
function formatNumber(value, digits) {
  if (!isFinite(value)) {
    return String(value);
  }
  if (value === 0) {
    return "0";
  }
  const magnitude = Math.abs(value);
  if (magnitude >= 1e15 || magnitude < 1e-6) {
    return value.toExponential(Math.max(1, digits - 1)).replace(/e([+-])(\d)$/, "e$10$2");
  }
  const rounded = Number(value.toPrecision(digits));
  return String(rounded);
}

function familyListing(verbose) {
  const lines = ["Supported families (unit_convert):"];
  for (const name of FAMILY_ORDER) {
    const family = FAMILIES[name];
    if (!family) {
      continue;
    }
    const canonical = [];
    const seen = {};
    for (const alias of Object.keys(family.units)) {
      const entry = family.units[alias];
      if (!entry || seen[entry[1]]) {
        continue;
      }
      seen[entry[1]] = true;
      canonical.push(entry[1]);
    }
    const line = "  " + name + " (base " + family.base + "): " + canonical.join(", ");
    lines.push(verbose ? line + " [also accepts: " + Object.keys(family.units).join(", ") + "]" : line);
  }
  const temperature = [];
  const seenTemperature = {};
  for (const alias of Object.keys(TEMPERATURE.units)) {
    const unit = TEMPERATURE.units[alias];
    if (seenTemperature[unit]) {
      continue;
    }
    seenTemperature[unit] = true;
    temperature.push(unit);
  }
  lines.push("  temperature (base K): " + temperature.join(", "));
  lines.push("Note: time uses mo = 30 d and yr = 365.25 d; currency is not supported.");
  return lines.join("\n");
}

export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "unit_convert",
      description: "Convert a measurement between units, offline and exactly. Families: length, mass, area, volume, temperature (affine, so 0 °C = 32 °F = 273.15 K), time, speed, pressure, energy, power, angle, frequency, data (decimal and binary prefixes kept distinct: 1 GiB = 1073741824 B, 1 GB = 1000000000 B), force and torque. Give `value` and `from`, and either `to` (a unit, or a comma-separated list) or action:\"list\" to see a family's units; action:\"families\" lists every family with none of the numbers needed. Converting across families is refused with the reason (length to mass has no answer), a genuinely ambiguous spelling such as \"p\" is refused with the spellings that would work, and a case-only difference such as \"mW\" (milliwatt) versus \"MW\" (megawatt) is refused rather than guessed. Time uses mo = 30 d and yr = 365.25 d; currency and logarithmic units (dB, dBm) are not supported.",
      parameters: {
        type: "object",
        properties: {
          value: { type: "number", description: "The number to convert. Omit it only for action:\"list\" or action:\"families\"." },
          from: { type: "string", description: "The unit the value is in, e.g. cm, kg, °F, MiB, km/h, psi. With action:\"list\" it selects the family." },
          to: { type: "string", description: "The unit to convert to, or several separated by commas (e.g. \"m, ft, in\")." },
          precision: { type: "number", description: "Significant digits for the answer (defaults to the extension setting)." },
          action: { type: "string", enum: ["convert", "list", "families"], description: "convert (default) converts a number; list shows the units of one family (give `from` to choose it); families lists every family." },
          verbose: { type: "boolean", description: "Also print the factor used, and the formula for a temperature conversion." },
        },
        required: ["from"],
      },
    },
  }],

  async onToolCall(ctx, call) {
    if (call.name !== "unit_convert") {
      return undefined;
    }
    const args = call.arguments || {};
    const action = String(args.action || "convert").toLowerCase();
    const verbose = args.verbose === undefined ? ctx.config.verbose === true : args.verbose === true;
    const digits = integer(args.precision === undefined ? ctx.config.precision : args.precision, 6, 1, 15);
    if (action === "families") {
      return { content: familyListing(ctx.config.showAliases === true) };
    }
    if (action === "list") {
      if (args.from === undefined || String(args.from).trim() === "") {
        return { content: familyListing(ctx.config.showAliases === true) };
      }
      const wanted = String(args.from).trim().toLowerCase();
      // A family name is the natural way to ask for a listing, so it is accepted
      // alongside a unit name.
      const source = FAMILIES[wanted]
        ? { family: wanted }
        : wanted === "temperature"
          ? { family: "temperature", temperature: true }
          : resolve(args.from);
      if (source.temperature) {
        const seen = {};
        const temperatureLines = [];
        for (const alias of Object.keys(TEMPERATURE.units)) {
          const unit = TEMPERATURE.units[alias];
          if (!seen[unit]) {
            seen[unit] = true;
            temperatureLines.push("  " + unit + " - " + TEMPERATURE.formula(unit));
          }
        }
        return { content: "temperature units (base K):\n" + temperatureLines.join("\n") };
      }
      const family = FAMILIES[source.family];
      const unitLines = ["units of " + source.family + " (base " + family.base + "), with the factor to the base:"];
      const seen = {};
      for (const alias of Object.keys(family.units)) {
        const entry = family.units[alias];
        if (!entry || seen[entry[1]]) {
          continue;
        }
        seen[entry[1]] = true;
        const others = aliasesFor(family, entry[1]);
        const note = ctx.config.showAliases === true && others.length > 0 ? "   (accepts: " + others.join(", ") + ")" : "";
        unitLines.push("  " + entry[1] + " = " + formatNumber(entry[0], 12) + " " + family.base + note);
      }
      return { content: unitLines.join("\n") };
    }
    // action === "convert"
    if (args.value === undefined || args.value === null || args.value === "") {
      throw new Error("unit_convert: a value is required to convert (use action:\"list\" to see the units)");
    }
    const value = Number(args.value);
    if (!isFinite(value)) {
      throw new Error("unit_convert: \"" + args.value + "\" is not a number");
    }
    const source = resolve(args.from);
    if (args.to === undefined || String(args.to).trim() === "") {
      throw new Error("unit_convert: give a unit to convert to, or action:\"list\" to see the options");
    }
    const targets = String(args.to).split(",").map((entry) => entry.trim()).filter((entry) => entry !== "");
    if (targets.length === 0) {
      throw new Error("unit_convert: give a unit to convert to");
    }
    const limit = integer(ctx.config.maxTargets, 12, 1, 50);
    if (targets.length > limit) {
      throw new Error("unit_convert: " + targets.length + " targets were asked for, over the limit of " + limit + "; raise maxTargets, or ask for fewer");
    }
    const lines = [];
    for (const target of targets) {
      const goal = resolve(target);
      if (goal.family !== source.family) {
        throw new Error("unit_convert: \"" + target + "\" measures " + goal.family + " and \"" + args.from + "\" measures " + source.family + "; these are different quantities, so there is no conversion");
      }
      if (goal.temperature) {
        const kelvin = TEMPERATURE.toKelvin(value, source.canonical);
        if (kelvin < 0) {
          throw new Error("unit_convert: " + formatNumber(value, digits) + " " + source.canonical + " is below absolute zero, which is not a temperature");
        }
        const converted = TEMPERATURE.fromKelvin(kelvin, goal.canonical);
        lines.push(formatNumber(value, digits) + " " + source.canonical + " = " + formatNumber(converted, digits) + " " + goal.canonical);
        if (verbose) {
          lines.push("   " + TEMPERATURE.formula(source.canonical) + "; " + TEMPERATURE.formula(goal.canonical) + " (through K)");
        }
        continue;
      }
      const base = value * source.factor;
      const converted = base / goal.factor;
      lines.push(formatNumber(value, digits) + " " + source.canonical + " = " + formatNumber(converted, digits) + " " + goal.canonical);
      if (verbose) {
        lines.push("   1 " + source.canonical + " = " + formatNumber(source.factor / goal.factor, 12) + " " + goal.canonical + " (through " + FAMILIES[source.family].base + ")");
      }
    }
    if (verbose && source.family === "time") {
      lines.push("note: mo is 30 days and yr is 365.25 days by definition here.");
    }
    if (verbose && source.family === "data") {
      lines.push("note: decimal prefixes (kB = 1000 B) and binary prefixes (KiB = 1024 B) are separate units.");
    }
    return { content: lines.join("\n") };
  },
};

function aliasesFor(family, canonical) {
  const result = [];
  for (const alias of Object.keys(family.units)) {
    const entry = family.units[alias];
    if (!entry) {
      continue;
    }
    if (entry[1] === canonical && alias !== canonical) {
      result.push(alias);
    }
  }
  return result;
}
