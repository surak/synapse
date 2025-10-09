from playwright.sync_api import sync_playwright, Page, expect

def run_verification():
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        page = browser.new_page()

        # Navigate to the /docs page
        page.goto("http://localhost:8080/docs")

        # Check for the title
        expect(page).to_have_title("Synapse Models")

        # Check for the swagger-ui element
        swagger_ui = page.locator("#swagger-ui")
        expect(swagger_ui).to_be_visible()

        # Wait for the spec to load by looking for an operation block
        page.wait_for_selector(".swagger-ui .opblock")

        # Take a screenshot
        page.screenshot(path="jules-scratch/verification/verification.png")

        browser.close()

if __name__ == "__main__":
    run_verification()