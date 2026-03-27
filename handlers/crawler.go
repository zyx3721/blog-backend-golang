package handlers

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"github.com/gin-gonic/gin"
	"github.com/go-shiori/go-readability"
)

// FetchArticleRequest 请求参数
type FetchArticleRequest struct {
	URL string `json:"url" binding:"required,url"`
}

// FetchArticleResponse 响应数据
type FetchArticleResponse struct {
	Title      string `json:"title"`
	Content    string `json:"content"`
	CoverImage string `json:"cover_image"`
	CategoryID string `json:"category_id"`
}

// FetchArticleByURL 通过URL抓取外部文章并转换为Markdown
func (h *Handlers) FetchArticleByURL(c *gin.Context) {
	var req FetchArticleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL provided"})
		return
	}

	// 1. 设置超时，防止请求挂起
	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	// 2. 发起 HTTP GET 请求
	httpReq, err := http.NewRequest("GET", req.URL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}

	// 添加基础请求头，模拟浏览器防止被拦截
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")

	resp, err := client.Do(httpReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to fetch URL: %v", err)})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Failed to fetch URL, status code: %d", resp.StatusCode)})
		return
	}

	// 3. 使用 goquery 提前处理 HTML 中的图片（解决懒加载和相对路径）
	parsedUrl, err := resp.Request.URL.Parse("")
	if err != nil {
		parsedUrl = resp.Request.URL
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to parse HTML document: %v", err)})
		return
	}

	var fallbackCover string
	lazyAttrs := []string{"data-src", "data-original", "data-lazy-src", "data-actualsrc", "origin-src", "data-src-retina", "v-lazy", "src-large"}
	doc.Find("img").Each(func(i int, s *goquery.Selection) {
		for _, attr := range lazyAttrs {
			if val, exists := s.Attr(attr); exists && val != "" {
				s.SetAttr("src", val)
				break 
			}
		}

		if src, exists := s.Attr("src"); exists && strings.TrimSpace(src) != "" {
			// 使用 Go 标准库的 ResolveReference 完美处理一切相对路径（包括无前缀的目录名、./、../、// 等）
			imgUrl, err := url.Parse(src)
			if err == nil {
				absUrl := parsedUrl.ResolveReference(imgUrl)
				src = absUrl.String()
				s.SetAttr("src", src)
			}

			// 获取第一张常规图片作为封面备选
			if fallbackCover == "" && strings.HasPrefix(src, "http") {
				if !strings.Contains(strings.ToLower(src), "icon") && !strings.Contains(strings.ToLower(src), "avatar") && !strings.Contains(strings.ToLower(src), "logo") {
					fallbackCover = src
				}
			}
		}
	})

	htmlStr, err := doc.Html()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate optimized HTML"})
		return
	}

	// 提取正文内容和标题
	article, err := readability.FromReader(strings.NewReader(htmlStr), parsedUrl)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to parse article content: %v", err)})
		return
	}

	// 如果没有提取到任何内容，报错
	if article.TextContent == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "No readable content found on this page"})
		return
	}

	// 4. 将 HTML 转换为 Markdown
	converter := md.NewConverter("", true, nil)
	
	// 为了克隆的博客文章更美观，可以在此添加一些原链接的引用
	markdown, err := converter.ConvertString(article.Content)
	if err != nil {
		// 如果转换失败，降级使用纯文本提取
		markdown = article.TextContent
	}

	// 追加原文来源信息，尊重版权
	sourceNotice := fmt.Sprintf("\n\n---\n> **说明**：本文通过 URL 克隆抓取。\n> **原文链接**：[%s](%s)\n> **抓取时间**：%s", article.Title, req.URL, time.Now().Format("2006-01-02 15:04:05"))
	markdownContent := markdown + sourceNotice

	// 5. 智能匹配分类
	matchedCategoryID := ""
	categories, err := h.repo.GetCategories()
	if err == nil && len(categories) > 0 {
		// 将文章标题和部分正文转为小写用于匹配
		searchTarget := strings.ToLower(article.Title + " " + article.Excerpt)
		
		for _, cat := range categories {
			catName := strings.ToLower(cat.Name)
			// 如果正文或标题包含分类名称（比如 "Go", "Vue", "前端"），则命中
			if strings.Contains(searchTarget, catName) {
				matchedCategoryID = cat.ID
				break // 找到一个匹配的就退出
			}
		}
	}

	// 6. 提取封面图片
	coverImage := article.Image
	if coverImage == "" {
		coverImage = fallbackCover
	}
	
	// 如果提取出来的封面是相对路径或双斜杠的缺省协议，做一层兜底修复
	if coverImage != "" {
		if strings.HasPrefix(coverImage, "//") {
			coverImage = "https:" + coverImage
		} else if strings.HasPrefix(coverImage, "/") {
			coverImage = fmt.Sprintf("%s://%s%s", parsedUrl.Scheme, parsedUrl.Host, coverImage)
		}
	}

	// 7. 返回给前端
	c.JSON(http.StatusOK, FetchArticleResponse{
		Title:      article.Title,
		Content:    markdownContent,
		CoverImage: coverImage,
		CategoryID: matchedCategoryID,
	})
}
