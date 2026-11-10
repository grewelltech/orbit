// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";

// The documentation + install site for ORBIT, deployed to GitHub Pages at
// the custom domain. `site` must match the served origin so canonical URLs
// and the sitemap are correct.
export default defineConfig({
  site: "https://orbit.grewelltech.com",
  integrations: [
    starlight({
      title: "ORBIT",
      description: "Load, fleet and conformance testing for 5G SA cores — one CLI, one dashboard.",
      social: {
        github: "https://github.com/grewelltech/orbit",
      },
      sidebar: [
        { label: "Install", link: "/install/" },
        { label: "Quickstart", link: "/quickstart/" },
        { label: "Deploying a responder", link: "/responder/" },
        {
          label: "Reference",
          items: [
            { label: "Usage", link: "https://github.com/grewelltech/orbit/blob/main/docs/USAGE.md", attrs: { target: "_blank" } },
            { label: "Architecture (ADRs)", link: "https://github.com/grewelltech/orbit/tree/main/docs/adr", attrs: { target: "_blank" } },
          ],
        },
      ],
    }),
  ],
});
