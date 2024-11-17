import { getColorScheme, registerVuetifyTheme } from "@/lib/darkmode";
import { createApp } from "vue";
import Vuetify from "vuetify";
import { createPinia } from "pinia";
import App from "./App.vue";
import router from "./routes";
import VueApexCharts from "vue-apexcharts";

// TODO:
//Vue.config.productionTip = process.env.NODE_ENV == "production";

const pinia = createPinia();
const app = createApp(App);

app.use(pinia);
app.use(router);
app.use(Vuetify, { theme: { dark: getColorScheme() === "dark" } });

app.use(Vuetify);
app.use(VueApexCharts);
const vuetifyInstance = new Vuetify({
  theme: { dark: getColorScheme() === "dark" },
});
registerVuetifyTheme(vuetifyInstance.framework);

app.mount("#app");

export default app;
